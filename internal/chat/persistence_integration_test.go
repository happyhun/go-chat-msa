//go:build integration

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"go-chat-msa/internal/shared/config"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	containerwait "github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

func TestJetStreamPersistenceRecovery(t *testing.T) {
	ctx := t.Context()
	natsContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Image:              "nats:2.14.6-alpine",
		ExposedPorts:       []string{"4222/tcp"},
		Cmd:                []string{"-c", "/etc/nats/test.conf"},
		HostConfigModifier: persistencePortBinding(t, "4222/tcp"),
		Files: []testcontainers.ContainerFile{{
			Reader:            strings.NewReader("port: 4222\njetstream { store_dir: /data/jetstream }\n"),
			ContainerFilePath: "/etc/nats/test.conf",
			FileMode:          0644,
		}},
		WaitingFor: containerwait.ForListeningPort("4222/tcp"),
		Started:    true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, natsContainer.Terminate(context.Background())) })
	endpoint, err := natsContainer.Endpoint(ctx, "")
	require.NoError(t, err)
	nc, err := nats.Connect("nats://"+endpoint, nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	mongoContainer, err := mongodb.Run(ctx, "mongo:7", testcontainers.WithHostConfigModifier(persistencePortBinding(t, "27017/tcp")))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mongoContainer.Terminate(context.Background())) })
	uri, err := mongoContainer.ConnectionString(ctx)
	require.NoError(t, err)
	db, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetServerSelectionTimeout(500*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Disconnect(context.Background())) })
	journal := true
	col := db.Database("chat_service").Collection("messages", options.Collection().SetWriteConcern(&writeconcern.WriteConcern{W: 1, Journal: &journal}))
	_, err = col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "senderId", Value: 1}, {Key: "clientMsgId", Value: 1}}, Options: options.Index().SetUnique(true)},
	})
	require.NoError(t, err)
	cfg := config.PersistenceConfig{Workers: 2, MaxBytes: 1 << 20, DLQMaxBytes: 1 << 20, MaxAckPending: 1000, WriteTimeout: time.Second, AckWait: 2 * time.Second}
	p, err := NewPersistence(ctx, nc, NewRepository(col), cfg, func(ctx context.Context) error { return db.Ping(ctx, readpref.Primary()) })
	require.NoError(t, err)
	room, sender := uuid.NewString(), uuid.NewString()
	makeMessage := func(content string) *Message {
		id, err := uuid.NewV7()
		require.NoError(t, err)
		sec, nsec := id.Time().UnixTime()
		return &Message{ID: id.String(), RoomID: room, SenderID: sender, ClientMsgID: uuid.NewString(), Content: content, Type: "chat", CreatedAt: time.Unix(sec, nsec)}
	}
	accepted := make(map[string]bool)
	publish := func(message *Message) {
		require.NoError(t, publishPersistenceMessage(ctx, p, message))
		accepted[message.ID] = true
	}
	republished, err := nc.SubscribeSync("room.msg." + room)
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	for i := range 20 {
		message := makeMessage(fmt.Sprintf("before worker %d", i))
		if i == 0 {
			require.NoError(t, publishWebSocketMessage(ctx, p, message))
			accepted[message.ID] = true
			continue
		}
		publish(message)
	}
	republishedMessage, err := republished.NextMsg(time.Second)
	require.NoError(t, err)
	require.Contains(t, string(republishedMessage.Data), `"timestamp":`)
	require.NotEmpty(t, republishedMessage.Header.Get("Nats-Sequence"))
	batch, err := p.consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
	require.NoError(t, err)
	for delivery := range batch.Messages() {
		var message Message
		require.NoError(t, json.Unmarshal(delivery.Data(), &message))
		setCreatedAtFromID(&message)
		require.Equal(t, []error{nil}, NewRepository(col).SaveBatch(ctx, []*Message{&message}))
	}
	require.NoError(t, batch.Error())
	stored, err := col.CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	require.EqualValues(t, 1, stored)
	beforeRestart, err := p.consumer.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, beforeRestart.NumAckPending)
	zero := time.Duration(0)
	require.NoError(t, natsContainer.Stop(ctx, &zero))
	require.NoError(t, natsContainer.Start(ctx))
	require.Eventually(t, nc.IsConnected, 10*time.Second, 100*time.Millisecond)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Run(workerCtx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	count := func() int64 {
		n, err := col.CountDocuments(ctx, bson.M{})
		if err != nil {
			return -1
		}
		return n
	}
	require.Eventually(t, func() bool { return count() == 20 }, 20*time.Second, 100*time.Millisecond)
	duplicate := makeMessage("duplicate")
	publish(duplicate)
	require.NoError(t, publishPersistenceMessage(ctx, p, duplicate))
	require.Eventually(t, func() bool { return count() == 21 }, 10*time.Second, 100*time.Millisecond)
	newIDDuplicate := *duplicate
	id, err := uuid.NewV7()
	require.NoError(t, err)
	newIDDuplicate.ID = id.String()
	require.NoError(t, publishPersistenceMessage(ctx, p, &newIDDuplicate))
	otherSender := *duplicate
	otherID, err := uuid.NewV7()
	require.NoError(t, err)
	otherSender.ID = otherID.String()
	otherSender.SenderID = uuid.NewString()
	publish(&otherSender)
	require.Eventually(t, func() bool { return count() == 22 }, 20*time.Second, 100*time.Millisecond)
	require.NoError(t, mongoContainer.Stop(ctx, &zero))
	for i := range 30 {
		publish(makeMessage(fmt.Sprintf("during mongo outage %d", i)))
	}
	require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.state == circuitOpen }, 15*time.Second, 100*time.Millisecond)
	info, err := p.consumer.Info(ctx)
	require.NoError(t, err)
	require.Positive(t, info.NumPending+uint64(info.NumAckPending))
	require.NoError(t, mongoContainer.Start(ctx))
	require.Eventually(t, func() bool { return count() == 52 }, 45*time.Second, 100*time.Millisecond)
	_, err = p.js.Publish(ctx, "chat.persist."+room, []byte("malformed"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		stream, e := p.js.Stream(ctx, dlqStream)
		if e != nil {
			return false
		}
		info, e := stream.Info(ctx)
		return e == nil && info.State.Msgs == 1
	}, 10*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool {
		info, e := p.consumer.Info(ctx)
		return e == nil && info.NumPending == 0 && info.NumAckPending == 0
	}, 10*time.Second, 100*time.Millisecond)
	cursor, err := col.Find(ctx, bson.M{})
	require.NoError(t, err)
	var saved []*Message
	require.NoError(t, cursor.All(ctx, &saved))
	require.Len(t, saved, len(accepted))
	for _, message := range saved {
		require.True(t, accepted[message.ID], "unexpected persisted ID %s", message.ID)
		delete(accepted, message.ID)
	}
	require.Empty(t, accepted)
}

func TestJetStreamPersistenceUpgradesExistingStreams(t *testing.T) {
	ctx := t.Context()
	natsContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Image:        "nats:2.14.6-alpine",
		ExposedPorts: []string{"4222/tcp"},
		Cmd:          []string{"-js"},
		WaitingFor:   containerwait.ForListeningPort("4222/tcp"),
		Started:      true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, natsContainer.Terminate(context.Background())) })
	endpoint, err := natsContainer.Endpoint(ctx, "")
	require.NoError(t, err)
	nc, err := nats.Connect("nats://" + endpoint)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: persistenceStream, Subjects: []string{"chat.persist.message"}, Retention: jetstream.WorkQueuePolicy, Storage: jetstream.FileStorage,
	})
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: dlqStream, Subjects: []string{"chat.persist.dlq"}, Storage: jetstream.FileStorage,
	})
	require.NoError(t, err)
	_, err = js.CreateConsumer(ctx, persistenceStream, jetstream.ConsumerConfig{
		Name: persistenceConsumer, Durable: persistenceConsumer, AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: "chat.persist.message",
	})
	require.NoError(t, err)
	_, err = js.Publish(ctx, "chat.persist.message", []byte("pending"))
	require.NoError(t, err)
	cfg := config.PersistenceConfig{Workers: 1, MaxBytes: 1 << 20, DLQMaxBytes: 1 << 20, MaxAckPending: 1000, WriteTimeout: time.Second, AckWait: 3 * time.Second}
	p, err := NewPersistence(ctx, nc, nil, cfg, nil)
	require.NoError(t, err)
	dlq, err := js.Stream(ctx, dlqStream)
	require.NoError(t, err)
	dlqInfo, err := dlq.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{dlqSubject}, dlqInfo.Config.Subjects)
	stream, err := js.Stream(ctx, persistenceStream)
	require.NoError(t, err)
	streamInfo, err := stream.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{persistenceSubject}, streamInfo.Config.Subjects)
	require.EqualValues(t, 1, streamInfo.State.Msgs)
	consumerInfo, err := p.consumer.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, persistenceSubject, consumerInfo.Config.FilterSubject)
	batch, err := p.consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
	require.NoError(t, err)
	for msg := range batch.Messages() {
		require.Equal(t, []byte("pending"), msg.Data())
		require.NoError(t, msg.Ack())
	}
	require.NoError(t, batch.Error())
}

func TestJetStreamCapacityPreservesPending(t *testing.T) {
	ctx := t.Context()
	ncContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Image:        "nats:2.14.6-alpine",
		ExposedPorts: []string{"4222/tcp"},
		Cmd:          []string{"-js"},
		WaitingFor:   containerwait.ForListeningPort("4222/tcp"),
		Started:      true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ncContainer.Terminate(context.Background())) })
	endpoint, err := ncContainer.Endpoint(ctx, "")
	require.NoError(t, err)
	nc, err := nats.Connect("nats://" + endpoint)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	cfg := config.PersistenceConfig{Workers: 1, MaxBytes: 2048, DLQMaxBytes: 128, MaxAckPending: 10, WriteTimeout: time.Second, AckWait: 3 * time.Second}
	p, err := NewPersistence(ctx, nc, nil, cfg, nil)
	require.NoError(t, err)
	id, err := uuid.NewV7()
	require.NoError(t, err)
	message := &Message{ID: id.String(), RoomID: uuid.NewString(), SenderID: uuid.NewString(), ClientMsgID: uuid.NewString(), Content: strings.Repeat("a", 1200), Type: "chat", CreatedAt: time.Now()}
	republished, err := nc.SubscribeSync("room.msg." + message.RoomID)
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	require.NoError(t, publishPersistenceMessage(ctx, p, message))
	_, err = republished.NextMsg(time.Second)
	require.NoError(t, err)
	id, err = uuid.NewV7()
	require.NoError(t, err)
	message.ID = id.String()
	message.ClientMsgID = uuid.NewString()
	require.Error(t, publishPersistenceMessage(ctx, p, message))
	_, err = republished.NextMsg(100 * time.Millisecond)
	require.ErrorIs(t, err, nats.ErrTimeout)
	stream, err := p.js.Stream(ctx, persistenceStream)
	require.NoError(t, err)
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.State.Msgs)
	batch, err := p.consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
	require.NoError(t, err)
	var delivery jetstream.Msg
	for msg := range batch.Messages() {
		delivery = msg
	}
	require.NoError(t, batch.Error())
	require.NotNil(t, delivery)
	p.deadLetter(ctx, delivery, "permanent", ErrInvalidMessage)
	info, err = stream.Info(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.State.Msgs)
	dlq, err := p.js.Stream(ctx, dlqStream)
	require.NoError(t, err)
	dlqInfo, err := dlq.Info(ctx)
	require.NoError(t, err)
	require.Zero(t, dlqInfo.State.Msgs)
	dlqInfo.Config.MaxBytes = 4096
	_, err = p.js.UpdateStream(ctx, dlqInfo.Config)
	require.NoError(t, err)
	batch, err = p.consumer.Fetch(1, jetstream.FetchMaxWait(35*time.Second))
	require.NoError(t, err)
	for msg := range batch.Messages() {
		delivery = msg
	}
	require.NoError(t, batch.Error())
	metadata, err := delivery.Metadata()
	require.NoError(t, err)
	require.EqualValues(t, 2, metadata.NumDelivered)
	p.deadLetter(ctx, delivery, "permanent", ErrInvalidMessage)
	require.Eventually(t, func() bool {
		info, err := stream.Info(ctx)
		return err == nil && info.State.Msgs == 0
	}, time.Second, 10*time.Millisecond)
	dlqInfo, err = dlq.Info(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, dlqInfo.State.Msgs)
}

func publishPersistenceMessage(ctx context.Context, p *Persistence, message *Message) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	msg := &nats.Msg{Subject: "chat.persist." + message.RoomID, Data: data, Header: nats.Header{}}
	msg.Header.Set("Gochat-Message-Id", message.ID)
	_, err = p.js.PublishMsg(ctx, msg, jetstream.WithMsgID(message.ID))
	return err
}

func publishWebSocketMessage(ctx context.Context, p *Persistence, message *Message) error {
	data, err := json.Marshal(struct {
		ID          string `json:"id"`
		RoomID      string `json:"room_id"`
		SenderID    string `json:"sender_id"`
		Content     string `json:"content"`
		ClientMsgID string `json:"client_msg_id"`
		Type        string `json:"type"`
		Timestamp   int64  `json:"timestamp"`
	}{
		ID:          message.ID,
		RoomID:      message.RoomID,
		SenderID:    message.SenderID,
		Content:     message.Content,
		ClientMsgID: message.ClientMsgID,
		Type:        message.Type,
		Timestamp:   message.CreatedAt.Unix(),
	})
	if err != nil {
		return err
	}
	_, err = p.js.Publish(ctx, "chat.persist."+message.RoomID, data, jetstream.WithMsgID(message.ID))
	return err
}

func persistencePortBinding(t *testing.T, port string) func(*container.HostConfig) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, hostPort, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	return func(cfg *container.HostConfig) {
		cfg.PortBindings = network.PortMap{network.MustParsePort(port): []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: hostPort}}}
	}
}
