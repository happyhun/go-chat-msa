//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
)

const (
	wsReadTimeout     = 5 * time.Second
	httpClientTimeout = 10 * time.Second
)

type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("status %d: %s", e.StatusCode, e.Body)
}

func (s *E2ESuite) cleanupDatabases(ctx context.Context) {
	s.cleanupKubernetesDatabases(ctx)
}

func (s *E2ESuite) signUp(ctx context.Context, username, password string) error {
	return s.makeRequest(ctx, "POST", "/users", map[string]string{
		"username": username,
		"password": password,
	}, nil)
}

func (s *E2ESuite) login(ctx context.Context, username, password string) (string, string, error) {
	var res map[string]any
	err := s.makeRequest(ctx, "POST", "/auth/token", map[string]string{
		"username": username,
		"password": password,
	}, &res)
	if err != nil {
		s.T().Logf("login error: %v", err)
		return "", "", err
	}
	acc, _ := res["access_token"].(string)
	uid, _ := res["user_id"].(string)
	if acc == "" {
		return "", "", fmt.Errorf("empty access token in response: %v", res)
	}
	return acc, uid, nil
}

func (s *E2ESuite) loginWithCookie(ctx context.Context, username, password string) (string, *http.Cookie, error) {
	reqBody := fmt.Sprintf(`{"username":"%s","password":"%s"}`, username, password)
	req, err := http.NewRequestWithContext(ctx, "POST", s.gatewayBaseURL+"/auth/token", strings.NewReader(reqBody))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: httpClientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("login failed: status %d", resp.StatusCode)
	}

	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", nil, err
	}

	token, _ := res["access_token"].(string)
	if token == "" {
		return "", nil, fmt.Errorf("empty access token in response")
	}

	var refreshCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}

	return token, refreshCookie, nil
}

func (s *E2ESuite) createRoom(ctx context.Context, token, name string) (string, error) {
	var res map[string]any
	err := s.makeRequest(ctx, "POST", "/rooms", map[string]any{
		"name":     name,
		"capacity": 100,
	}, &res, token)
	if err != nil {
		return "", err
	}
	return res["room_id"].(string), nil
}

func (s *E2ESuite) makeRequest(ctx context.Context, method, path string, body any, result any, tokens ...string) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("body 마샬링 실패: %w", err)
		}
		bodyReader = strings.NewReader(string(b))
	}

	req, err := http.NewRequestWithContext(ctx, method, s.gatewayBaseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("요청 생성 실패: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if len(tokens) > 0 && tokens[0] != "" {
		req.Header.Set("Authorization", "Bearer "+tokens[0])
	}

	client := &http.Client{Timeout: httpClientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("요청 전송 실패: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("응답 바디 읽기 실패: %w", err)
	}

	if resp.StatusCode >= 400 {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	if result != nil {
		if err := json.Unmarshal(bodyBytes, result); err != nil {
			return fmt.Errorf("응답 언마샬링 실패: %w, body: %s", err, string(bodyBytes))
		}
	}
	return nil
}

func (s *E2ESuite) sendMessage(conn *websocket.Conn, content string) error {
	return conn.WriteJSON(map[string]string{
		"type": "chat", "content": content, "client_msg_id": uuid.NewString(),
	})
}

func (s *E2ESuite) getWSTicket(ctx context.Context, token string) (string, error) {
	var res struct {
		Ticket string `json:"ticket"`
	}
	if err := s.makeRequest(ctx, "POST", "/auth/ws-ticket", nil, &res, token); err != nil {
		return "", err
	}
	if res.Ticket == "" {
		return "", fmt.Errorf("응답에 ticket이 없습니다")
	}
	return res.Ticket, nil
}

func (s *E2ESuite) getWSURL(roomID, ticket string) string {
	base := s.wsBaseURL

	if strings.HasPrefix(base, "http://") {
		base = "ws://" + base[7:]
	}
	return fmt.Sprintf("%s/ws?room_id=%s&ticket=%s", base, roomID, ticket)
}

func (s *E2ESuite) dialWS(ctx context.Context, token, roomID string) (*websocket.Conn, *http.Response, error) {
	ticket, err := s.getWSTicket(ctx, token)
	if err != nil {
		if httpErr, ok := errors.AsType[*HTTPError](err); ok {
			return nil, &http.Response{StatusCode: httpErr.StatusCode}, err
		}
		return nil, nil, err
	}

	return websocket.DefaultDialer.Dial(s.getWSURL(roomID, ticket), nil)
}

func (s *E2ESuite) waitForWSMessage(ctx context.Context, conn *websocket.Conn, msgType string, contentMatch string, timeout time.Duration) (map[string]any, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("read deadline 설정 실패: %w", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("컨텍스트 종료: %w", ctx.Err())
		}

		_, p, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("메시지 수신 대기 시간 초과 또는 연결 오류 (timeout=%v): %w", timeout, err)
		}

		var msg map[string]any
		if err := json.Unmarshal(p, &msg); err != nil {
			continue
		}

		s.NotContains(msg, "frame_no")

		if msgType != "" && msg["type"] != msgType {
			continue
		}

		if contentMatch != "" {
			content, ok := msg["content"].(string)
			if !ok || !strings.Contains(content, contentMatch) {
				continue
			}
		}

		return msg, nil
	}
}

func (s *E2ESuite) assertHTTPError(err error, expectedStatus int) {
	s.Require().Error(err, "에러가 발생해야 함")
	if httpErr, ok := errors.AsType[*HTTPError](err); ok {
		s.Equal(expectedStatus, httpErr.StatusCode, "상태 코드 불일치")
	} else {

		s.Contains(err.Error(), fmt.Sprintf("status %d", expectedStatus))
	}
}

func (s *E2ESuite) generateUniqueUsername(prefix string) string {
	const maxLen = 12
	randBytes := (maxLen - len(prefix)) / 2
	b := make([]byte, randBytes)
	rand.Read(b)
	return fmt.Sprintf("%s%x", prefix, b)
}

func (s *E2ESuite) requirePersistedMessages(ctx context.Context, token, roomID string, accepted map[string]bool) {
	s.T().Helper()
	s.Require().EventuallyWithT(func(c *assert.CollectT) {
		var response struct {
			Messages []map[string]any `json:"messages"`
		}
		if !assert.NoError(c, s.makeRequest(ctx, http.MethodGet, "/rooms/"+roomID+"/messages?limit=100", nil, &response, token)) {
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
}

func (s *E2ESuite) mongoProcessIdentity(ctx context.Context) (string, error) {
	return s.kubectlOutput(ctx, "-n", s.namespace, "get", "pods", "-l", "app.kubernetes.io/name=mongo", "-o",
		`jsonpath={range .items[*]}{.metadata.uid}{"/"}{.status.containerStatuses[?(@.name=="mongo")].containerID}{end}`)
}

func (s *E2ESuite) mongoProxyEnabled(ctx context.Context) (bool, error) {
	out, err := s.kubectlOutput(ctx, "-n", s.namespace, "exec", "deployment/mongo", "-c", "mongo-proxy", "--", "/toxiproxy-cli", "list")
	if err != nil {
		return false, fmt.Errorf("MongoDB fault proxy unavailable; run make test-up: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == "mongo" {
			switch fields[3] {
			case "enabled":
				return true, nil
			case "disabled":
				return false, nil
			}
		}
	}
	return false, fmt.Errorf("unexpected MongoDB proxy state: %q", out)
}

func (s *E2ESuite) setMongoProxyEnabled(ctx context.Context, enabled bool) error {
	current, err := s.mongoProxyEnabled(ctx)
	if err != nil || current == enabled {
		return err
	}
	if err := s.runKubectl(ctx, "-n", s.namespace, "exec", "deployment/mongo", "-c", "mongo-proxy", "--", "/toxiproxy-cli", "toggle", "mongo"); err != nil {
		return err
	}
	current, err = s.mongoProxyEnabled(ctx)
	if err != nil {
		return err
	}
	if current != enabled {
		return fmt.Errorf("MongoDB proxy enabled=%t, want %t", current, enabled)
	}
	return nil
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
