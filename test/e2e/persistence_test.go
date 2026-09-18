//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
)

func (s *E2ESuite) TestScenario_17_DurableAcceptanceAndRestartRecovery() {
	ctx := s.T().Context()
	password := "SecurePass123!"
	alice, bob := s.generateUniqueUsername("da"), s.generateUniqueUsername("db")
	for _, username := range []string{alice, bob} {
		s.Require().NoError(s.signUp(ctx, username, password))
	}
	aliceToken, _, err := s.login(ctx, alice, password)
	s.Require().NoError(err)
	bobToken, _, err := s.login(ctx, bob, password)
	s.Require().NoError(err)
	roomID, err := s.createRoom(ctx, aliceToken, "Durable recovery")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, http.MethodPut, "/rooms/"+roomID+"/members/me", nil, nil, bobToken))
	aliceConn, _, err := s.dialWS(ctx, aliceToken, roomID)
	s.Require().NoError(err)
	defer func() {
		if aliceConn != nil {
			_ = aliceConn.Close()
		}
	}()
	bobConn, _, err := s.dialWS(ctx, bobToken, roomID)
	s.Require().NoError(err)
	defer func() { _ = bobConn.Close() }()
	clientID := uuid.NewString()
	accepted := make(map[string]bool)
	send := func(conn *websocket.Conn, content, clientID string) string {
		s.Require().NoError(conn.WriteJSON(map[string]string{"type": "chat", "content": content, "client_msg_id": clientID}))
		message, err := s.waitForWSMessage(ctx, aliceConn, "chat", content, 10*time.Second)
		s.Require().NoError(err)
		s.Require().Equal(clientID, message["client_msg_id"])
		id := messageID(message)
		s.Require().NotEmpty(id)
		accepted[id] = true
		return id
	}
	firstID := send(aliceConn, "alice before outage", clientID)
	s.Require().NoError(aliceConn.WriteJSON(map[string]string{"type": "chat", "content": "alice before outage", "client_msg_id": clientID}))
	s.Require().NotEqual(firstID, send(bobConn, "bob before outage", clientID))
	s.Require().NoError(aliceConn.Close())
	s.Require().NoError(bobConn.Close())
	originalReplicas := make(map[string]string)
	for _, name := range []string{"mongo", "chat-service"} {
		replicas, err := s.kubectlOutput(ctx, "-n", s.namespace, "get", "deployment/"+name, "-o", "jsonpath={.spec.replicas}")
		s.Require().NoError(err)
		originalReplicas[name] = replicas
	}
	restore := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for name, replicas := range originalReplicas {
			if err := s.runKubectl(cleanupCtx, "-n", s.namespace, "scale", "deployment/"+name, "--replicas="+replicas); err != nil {
				s.T().Logf("restore: %v", err)
			}
		}
	}
	s.T().Cleanup(restore)
	for _, scenario := range []string{"mongo-recovery", "worker-restart", "worker-scale-in", "nats-restart"} {
		aliceConn, _, err = s.dialWS(ctx, aliceToken, roomID)
		s.Require().NoError(err)
		s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "scale", "deployment/mongo", "--replicas=0"))
		s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name=mongo", "--timeout=60s"))
		for i := range 8 {
			send(aliceConn, fmt.Sprintf("%s-%d", scenario, i), uuid.NewString())
			time.Sleep(300 * time.Millisecond)
		}
		if scenario != "nats-restart" {
			s.Require().NoError(aliceConn.Close())
		}
		counts, err := s.persistenceStreamCounts(ctx)
		s.Require().NoError(err)
		s.Require().GreaterOrEqual(counts["CHAT_PERSIST"], uint64(8))
		s.Require().Zero(counts["CHAT_PERSIST_DLQ"])
		switch scenario {
		case "worker-restart":
			s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "rollout", "restart", "deployment/chat-service"))
			s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "rollout", "status", "deployment/chat-service", "--timeout=180s"))
		case "worker-scale-in":
			for _, replicas := range []int{3, 1} {
				s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "scale", "deployment/chat-service", fmt.Sprintf("--replicas=%d", replicas)))
				s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "rollout", "status", "deployment/chat-service", "--timeout=180s"))
				s.requireDeploymentReadyReplicas(ctx, "chat-service", replicas)
			}
		case "nats-restart":
			pvc, err := s.kubectlOutput(ctx, "-n", s.namespace, "get", "pvc/data-nats-0", "-o", "jsonpath={.metadata.uid}")
			s.Require().NoError(err)
			s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "delete", "pod/nats-0"))
			s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "rollout", "status", "statefulset/nats", "--timeout=180s"))
			restoredPVC, err := s.kubectlOutput(ctx, "-n", s.namespace, "get", "pvc/data-nats-0", "-o", "jsonpath={.metadata.uid}")
			s.Require().NoError(err)
			s.Require().Equal(pvc, restoredPVC)
			s.Require().NoError(aliceConn.SetReadDeadline(time.Now().Add(10 * time.Second)))
			_, _, err = aliceConn.ReadMessage()
			s.Require().True(websocket.IsCloseError(err, websocket.CloseServiceRestart), "old session must close after NATS disconnect: %v", err)
			s.Require().EventuallyWithT(func(c *assert.CollectT) {
				aliceConn, _, err = s.dialWS(ctx, aliceToken, roomID)
				assert.NoError(c, err)
			}, 30*time.Second, time.Second)

			counts, err = s.persistenceStreamCounts(ctx)
			s.Require().NoError(err)
			s.Require().GreaterOrEqual(counts["CHAT_PERSIST"], uint64(8))
		}
		recoveryStarted := time.Now()
		s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "scale", "deployment/mongo", "--replicas=1"))
		s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "rollout", "status", "deployment/mongo", "--timeout=180s"))
		if scenario == "nats-restart" {
			send(aliceConn, "live after NATS recovery", uuid.NewString())
			s.Require().NoError(aliceConn.Close())
		}
		s.Require().EventuallyWithT(func(c *assert.CollectT) {
			var response struct {
				Messages []map[string]any `json:"messages"`
			}
			if !assert.NoError(c, s.makeRequest(ctx, http.MethodGet, "/rooms/"+roomID+"/messages?limit=100", nil, &response, aliceToken)) {
				return
			}
			actual := make(map[string]bool)
			for _, message := range response.Messages {
				actual[messageID(message)] = true
			}
			assert.Len(c, response.Messages, len(accepted))
			assert.Equal(c, accepted, actual)
			counts, err := s.persistenceStreamCounts(ctx)
			if assert.NoError(c, err) {
				assert.Zero(c, counts["CHAT_PERSIST"])
				assert.Zero(c, counts["CHAT_PERSIST_DLQ"])
			}
		}, 90*time.Second, time.Second)
		s.T().Logf("%s: accepted=%d, missing=0, duplicate=0, pending=0, DLQ=0, recovery=%s", scenario, len(accepted), time.Since(recoveryStarted).Round(time.Millisecond))
	}
}

func (s *E2ESuite) persistenceStreamCounts(ctx context.Context) (map[string]uint64, error) {
	out, err := s.kubectlOutput(ctx, "-n", s.namespace, "exec", "pod/nats-0", "-c", "nats", "--", "wget", "-qO-", "http://127.0.0.1:8222/jsz?streams=true")
	if err != nil {
		return nil, err
	}
	var response struct {
		Accounts []struct {
			Streams []struct {
				Name  string `json:"name"`
				State struct {
					Messages uint64 `json:"messages"`
				} `json:"state"`
			} `json:"stream_detail"`
		} `json:"account_details"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, err
	}
	counts := make(map[string]uint64)
	for _, account := range response.Accounts {
		for _, stream := range account.Streams {
			counts[stream.Name] = stream.State.Messages
		}
	}
	for _, name := range []string{"CHAT_PERSIST", "CHAT_PERSIST_DLQ"} {
		if _, ok := counts[name]; !ok {
			return nil, fmt.Errorf("stream %s missing from monitoring response", name)
		}
	}
	return counts, nil
}
