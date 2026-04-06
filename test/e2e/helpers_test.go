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

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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
	s.cleanupPostgres(ctx)
	s.cleanupMongo(ctx)
}

func (s *E2ESuite) cleanupPostgres(ctx context.Context) {
	connStr, err := s.postgres.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		s.T().Logf("cleanup: postgres connection string error: %v", err)
		return
	}

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		s.T().Logf("cleanup: postgres connect error: %v", err)
		return
	}
	defer conn.Close(ctx)

	if _, err = conn.Exec(ctx, "TRUNCATE TABLE users, rooms, room_members RESTART IDENTITY CASCADE;"); err != nil {
		s.T().Logf("cleanup: postgres truncate error: %v", err)
	}
}

func (s *E2ESuite) cleanupMongo(ctx context.Context) {
	endpoint, err := s.mongo.ConnectionString(ctx)
	if err != nil {
		s.T().Logf("cleanup: mongo connection string error: %v", err)
		return
	}

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(endpoint))
	if err != nil {
		s.T().Logf("cleanup: mongo connect error: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	if err = client.Database("chat_service").Drop(ctx); err != nil {
		s.T().Logf("cleanup: mongo drop error: %v", err)
	}
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
	s.T().Logf("login response: %v", res)

	acc, _ := res["access_token"].(string)
	uid, _ := res["user_id"].(string)
	if acc == "" {
		return "", "", fmt.Errorf("empty access token in response: %v", res)
	}
	return acc, uid, nil
}

func (s *E2ESuite) loginWithCookie(ctx context.Context, username, password string) (string, *http.Cookie, error) {
	reqBody := fmt.Sprintf(`{"username":"%s","password":"%s"}`, username, password)
	req, _ := http.NewRequestWithContext(ctx, "POST", s.gatewayBaseURL+"/auth/token", strings.NewReader(reqBody))
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

func (s *E2ESuite) getWSTicket(ctx context.Context, token string) (string, error) {
	wsBase := s.wsBaseURL

	if strings.HasPrefix(wsBase, "ws://") {
		wsBase = "http://" + wsBase[5:]
	}

	req, err := http.NewRequestWithContext(ctx, "POST", wsBase+"/ws/ticket", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: httpClientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("티켓 발급 요청 실패: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("응답 언마샬링 실패: %w", err)
	}
	ticket, ok := res["ticket"].(string)
	if !ok {
		return "", fmt.Errorf("응답에 ticket이 없습니다")
	}
	return ticket, nil
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
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
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
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
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
