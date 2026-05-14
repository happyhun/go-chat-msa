//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stretchr/testify/assert"
)

func (s *E2ESuite) TestScenario_14_K8s_FixedReplicasReadinessAndMembership() {
	if !s.isKubernetes() {
		s.T().Skip("K8s fixed replica scenario")
	}
	ctx := s.T().Context()

	for _, name := range []string{"api-gateway", "ws-gateway", "websocket-service", "user-service", "chat-service"} {
		s.requireDeploymentReadyReplicas(ctx, name, 2)
	}
	s.requireDeploymentReadyReplicas(ctx, "frontend", 1)

	keys := s.waitForRedisKeyCount(ctx, "wss:member:*", 2, 20*time.Second, 500*time.Millisecond)
	s.Require().Len(keys, 2, "two websocket-service pods must be registered in Redis membership")
}

func (s *E2ESuite) TestScenario_15_K8s_RoomSequenceIsStrictlyIncreasing() {
	if !s.isKubernetes() {
		s.T().Skip("K8s fixed replica scenario")
	}
	ctx := s.T().Context()

	alice := s.generateUniqueUsername("sa")
	bob := s.generateUniqueUsername("sb")
	password := "SecurePass123!"
	s.Require().NoError(s.signUp(ctx, alice, password))
	s.Require().NoError(s.signUp(ctx, bob, password))
	aliceToken, _, err := s.login(ctx, alice, password)
	s.Require().NoError(err)
	bobToken, _, err := s.login(ctx, bob, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, aliceToken, "K8s Sequence Room")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, bobToken))

	aliceConn, _, err := s.dialWS(ctx, aliceToken, roomID)
	s.Require().NoError(err)
	defer aliceConn.Close()

	bobConn, _, err := s.dialWS(ctx, bobToken, roomID)
	s.Require().NoError(err)
	defer bobConn.Close()

	const totalMessages = 8
	sequences := make([]int64, 0, totalMessages)
	for i := 1; i <= totalMessages; i++ {
		content := fmt.Sprintf("k8s-seq-msg-%02d", i)
		s.Require().NoError(bobConn.WriteJSON(map[string]string{
			"type":          "chat",
			"content":       content,
			"client_msg_id": fmt.Sprintf("k8s-seq-%02d", i),
		}))

		msg, err := s.waitForWSMessage(ctx, aliceConn, "chat", content, 10*time.Second)
		s.Require().NoError(err)
		sequences = append(sequences, sequenceNumber(msg))
	}

	for i := 1; i < len(sequences); i++ {
		s.Require().Greater(sequences[i], sequences[i-1], "room sequence must strictly increase")
	}
}

func (s *E2ESuite) TestScenario_16_K8s_ReconnectCatchUpFromDatabaseHistory() {
	if !s.isKubernetes() {
		s.T().Skip("K8s fixed replica scenario")
	}
	ctx := s.T().Context()

	alice := s.generateUniqueUsername("ka")
	bob := s.generateUniqueUsername("kb")
	carol := s.generateUniqueUsername("kc")
	password := "SecurePass123!"
	s.Require().NoError(s.signUp(ctx, alice, password))
	s.Require().NoError(s.signUp(ctx, bob, password))
	s.Require().NoError(s.signUp(ctx, carol, password))
	aliceToken, _, err := s.login(ctx, alice, password)
	s.Require().NoError(err)
	bobToken, _, err := s.login(ctx, bob, password)
	s.Require().NoError(err)
	carolToken, _, err := s.login(ctx, carol, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, aliceToken, "K8s CatchUp Room")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, bobToken))
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, carolToken))

	aliceConn, _, err := s.dialWS(ctx, aliceToken, roomID)
	s.Require().NoError(err)
	defer aliceConn.Close()
	carolConn, _, err := s.dialWS(ctx, carolToken, roomID)
	s.Require().NoError(err)
	defer carolConn.Close()

	const missedMessages = 4
	for i := 1; i <= missedMessages; i++ {
		content := fmt.Sprintf("k8s-missed-%02d", i)
		s.Require().NoError(aliceConn.WriteJSON(map[string]string{
			"type":          "chat",
			"content":       content,
			"client_msg_id": fmt.Sprintf("k8s-missed-%02d", i),
		}))
		_, err = s.waitForWSMessage(ctx, carolConn, "chat", content, 10*time.Second)
		s.Require().NoError(err)
	}

	var syncRes struct {
		Messages []map[string]any `json:"messages"`
	}
	s.Require().EventuallyWithT(func(c *assert.CollectT) {
		err := s.makeRequest(ctx, "GET", fmt.Sprintf("/rooms/%s/messages?last_seq=0&limit=10", roomID), nil, &syncRes, bobToken)
		if !assert.NoError(c, err) {
			return
		}
		assert.GreaterOrEqual(c, len(messagesWithContentPrefix(syncRes.Messages, "k8s-missed-")), missedMessages)
	}, 10*time.Second, 200*time.Millisecond)

	missed := messagesWithContentPrefix(syncRes.Messages, "k8s-missed-")
	s.Require().GreaterOrEqual(len(missed), missedMessages)
	for i := 1; i <= missedMessages; i++ {
		s.Require().Equal(fmt.Sprintf("k8s-missed-%02d", i), missed[i-1]["content"])
	}
}

func (s *E2ESuite) waitForRedisKeyCount(ctx context.Context, pattern string, expected int, timeout, interval time.Duration) []string {
	deadline := time.Now().Add(timeout)
	var keys []string
	for time.Now().Before(deadline) {
		keys = s.redisKeys(ctx, pattern)
		if len(keys) == expected {
			return keys
		}
		time.Sleep(interval)
	}
	return keys
}

func sequenceNumber(msg map[string]any) int64 {
	switch v := msg["sequence_number"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

func messagesWithContentPrefix(messages []map[string]any, prefix string) []map[string]any {
	filtered := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		content, ok := msg["content"].(string)
		if ok && strings.HasPrefix(content, prefix) {
			filtered = append(filtered, msg)
		}
	}
	return filtered
}
