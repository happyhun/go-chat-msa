//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
)

func (s *E2ESuite) TestScenario_01_AliceAndBob_Lifecycle() {
	s.T().Log(">>> starting TestScenario_01_AliceAndBob_Lifecycle")
	ctx := s.T().Context()

	alice := s.generateUniqueUsername("al")
	bob := s.generateUniqueUsername("bo")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, alice, password))
	s.Require().NoError(s.signUp(ctx, bob, password))
	aliceToken, _, err := s.login(ctx, alice, password)
	s.Require().NoError(err)
	bobToken, _, err := s.login(ctx, bob, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, aliceToken, "E2E Lobby")
	s.Require().NoError(err)

	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, bobToken))

	aliceConn, _, err := s.dialWS(ctx, aliceToken, roomID)
	s.Require().NoError(err)
	defer aliceConn.Close()

	bobConn, _, err := s.dialWS(ctx, bobToken, roomID)
	s.Require().NoError(err)
	defer bobConn.Close()

	content := "Hello Alice, welcome to the MSA world!"
	s.Require().NoError(bobConn.WriteJSON(map[string]string{
		"type":          "chat",
		"content":       content,
		"client_msg_id": "m1",
	}))

	msg, err := s.waitForWSMessage(ctx, aliceConn, "chat", content, wsReadTimeout)
	s.Require().NoError(err, "Alice should receive Bob's message")
	s.Equal(content, msg["content"])
	s.NotEmpty(msg["sequence_number"], "Sequence number must be assigned by server")
}

func (s *E2ESuite) TestScenario_02_Auth_TokenRefresh() {
	s.T().Log(">>> starting TestScenario_02_Auth_TokenRefresh")
	ctx := s.T().Context()

	user := s.generateUniqueUsername("refresh")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, user, password))

	oldToken, refreshCookie, err := s.loginWithCookie(ctx, user, password)
	s.Require().NoError(err)
	s.Require().NotNil(refreshCookie, "Refresh token cookie should be provided")

	time.Sleep(1 * time.Second)

	client := &http.Client{Timeout: httpClientTimeout}
	req, _ := http.NewRequestWithContext(ctx, "POST", s.gatewayBaseURL+"/auth/refresh", nil)
	req.AddCookie(refreshCookie)
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode, "Refresh token should successfully issue new access token")

	var res map[string]any
	json.NewDecoder(resp.Body).Decode(&res)
	newToken := res["access_token"].(string)

	s.NotEmpty(newToken)
	s.NotEqual(oldToken, newToken, "Access token should be rotated")
}

func (s *E2ESuite) TestScenario_03_Auth_Logout() {
	s.T().Log(">>> starting TestScenario_03_Auth_Logout")
	ctx := s.T().Context()

	user := s.generateUniqueUsername("logout")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, user, password))

	_, refreshCookie, err := s.loginWithCookie(ctx, user, password)
	s.Require().NoError(err)
	s.Require().NotNil(refreshCookie, "Refresh token cookie should be provided")

	client := &http.Client{Timeout: httpClientTimeout}
	logoutReq, _ := http.NewRequestWithContext(ctx, "DELETE", s.gatewayBaseURL+"/auth/token", nil)
	logoutReq.AddCookie(refreshCookie)
	logoutResp, err := client.Do(logoutReq)
	s.Require().NoError(err)
	defer logoutResp.Body.Close()

	s.Require().Equal(http.StatusNoContent, logoutResp.StatusCode, "Logout should return 204")

	cookieCleared := false
	for _, c := range logoutResp.Cookies() {
		if c.Name == "refresh_token" && c.MaxAge < 0 {
			cookieCleared = true
			break
		}
	}
	s.Require().True(cookieCleared, "Refresh token cookie should be expired after logout")

	refreshReq, _ := http.NewRequestWithContext(ctx, "POST", s.gatewayBaseURL+"/auth/refresh", nil)
	refreshReq.AddCookie(refreshCookie)
	refreshResp, err := client.Do(refreshReq)
	s.Require().NoError(err)
	defer refreshResp.Body.Close()

	s.Require().Equal(http.StatusUnauthorized, refreshResp.StatusCode, "Revoked refresh token should be rejected")
}

func (s *E2ESuite) TestScenario_04_WS_UnauthorizedAccess() {
	s.T().Log(">>> starting TestScenario_04_WS_UnauthorizedAccess")
	ctx := s.T().Context()

	u1 := s.generateUniqueUsername("unauth1")
	u2 := s.generateUniqueUsername("unauth2")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, u1, password))
	s.Require().NoError(s.signUp(ctx, u2, password))

	t1, _, err := s.login(ctx, u1, password)
	s.Require().NoError(err)
	t2, _, err := s.login(ctx, u2, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, t1, "Private Access")
	s.Require().NoError(err)

	_, resp, err := s.dialWS(ctx, t2, roomID)
	s.Require().Error(err, "Unjoined user should fail to connect WS")
	if resp != nil {
		s.Require().True(
			resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode == http.StatusUnauthorized ||
				resp.StatusCode == http.StatusInternalServerError ||
				resp.StatusCode == http.StatusBadRequest,
			"Expected error status: %d", resp.StatusCode)
	}
}

func (s *E2ESuite) TestScenario_05_BurstChatting_Concurrency() {
	s.T().Log(">>> starting TestScenario_05_BurstChatting_Concurrency")
	ctx := s.T().Context()

	manager := s.generateUniqueUsername("mgr")
	password := "SecurePass123!"
	s.Require().NoError(s.signUp(ctx, manager, password))
	token, _, err := s.login(ctx, manager, password)
	s.Require().NoError(err)
	roomID, err := s.createRoom(ctx, token, "High Traffic Room")
	s.Require().NoError(err)

	numClients := 10
	conns := make([]*websocket.Conn, numClients)
	g, gCtx := errgroup.WithContext(ctx)

	var mu sync.Mutex
	for i := range numClients {
		time.Sleep(500 * time.Millisecond)
		g.Go(func() error {
			user := s.generateUniqueUsername(fmt.Sprintf("u%d", i))
			if err := s.signUp(gCtx, user, password); err != nil {
				return err
			}
			tok, _, err := s.login(gCtx, user, password)
			if err != nil {
				return err
			}
			if err := s.makeRequest(gCtx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, tok); err != nil {
				return err
			}
			conn, _, err := s.dialWS(gCtx, tok, roomID)
			if err != nil {
				return err
			}

			mu.Lock()
			conns[i] = conn
			mu.Unlock()
			return nil
		})
	}
	s.Require().NoError(g.Wait(), "Massive join and connection failed")
	defer func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	}()

	msgPerClient := 5
	totalExpected := numClients * msgPerClient

	for i, conn := range conns {
		g.Go(func() error {
			for j := range msgPerClient {
				if err := conn.WriteJSON(map[string]string{
					"type":          "chat",
					"content":       fmt.Sprintf("burst-from-%d", i),
					"client_msg_id": fmt.Sprintf("msg-%d-%d", i, j),
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	s.Require().NoError(g.Wait(), "Burst message sending failed")

	lastConn := conns[numClients-1]
	receivedCount := 0

	s.Eventually(func() bool {
		lastConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			_, p, err := lastConn.ReadMessage()
			if err != nil {
				return receivedCount >= totalExpected
			}
			var m map[string]any
			if err := json.Unmarshal(p, &m); err == nil && m["type"] == "chat" {
				receivedCount++
			}
			if receivedCount >= totalExpected {
				return true
			}
		}
	}, 15*time.Second, 1*time.Second, "All broadcasted messages should be received by all clients")

	s.T().Logf("Concurrency Check PASSED: Total %d messages arrived correctly", receivedCount)
}

func (s *E2ESuite) TestScenario_06_Message_Pagination() {
	s.T().Log(">>> starting TestScenario_06_Message_Pagination")
	ctx := s.T().Context()

	u1 := s.generateUniqueUsername("pageu1")
	password := "SecurePass123!"
	s.Require().NoError(s.signUp(ctx, u1, password))
	t1, _, err := s.login(ctx, u1, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, t1, "Paging Room")
	s.Require().NoError(err)

	conn, _, err := s.dialWS(ctx, t1, roomID)
	s.Require().NoError(err)

	s.waitForWSMessage(ctx, conn, "system", "", 5*time.Second)

	totalMsgs := 15
	for i := 1; i <= totalMsgs; i++ {
		conn.WriteJSON(map[string]string{
			"type":          "chat",
			"content":       fmt.Sprintf("page-msg-%d", i),
			"client_msg_id": fmt.Sprintf("cm-%d", i),
		})
		s.waitForWSMessage(ctx, conn, "chat", fmt.Sprintf("page-msg-%d", i), 5*time.Second)

		time.Sleep(600 * time.Millisecond)
	}
	conn.Close()

	time.Sleep(500 * time.Millisecond)

	var res1 struct {
		Messages []map[string]any `json:"messages"`
	}
	err = s.makeRequest(ctx, "GET", fmt.Sprintf("/rooms/%s/messages?limit=10", roomID), nil, &res1, t1)
	s.Require().NoError(err)
	s.Require().Len(res1.Messages, 10, "Should return exactly 10 latest messages")

	firstSeqVal := res1.Messages[0]["sequence_number"]
	s.Require().NotNil(firstSeqVal, "Messages must include valid sequence_number")
}

func (s *E2ESuite) TestScenario_07_MessageRecovery_Recovery() {
	s.T().Log(">>> starting TestScenario_07_MessageRecovery_Recovery")
	ctx := s.T().Context()

	alice := s.generateUniqueUsername("a")
	bob := s.generateUniqueUsername("b")
	password := "SecurePass123!"
	s.Require().NoError(s.signUp(ctx, alice, password))
	s.Require().NoError(s.signUp(ctx, bob, password))
	aTok, _, err := s.login(ctx, alice, password)
	s.Require().NoError(err)
	bTok, _, err := s.login(ctx, bob, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, aTok, "Resilience Room")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, bTok))

	bConn, _, err := s.dialWS(ctx, bTok, roomID)
	s.Require().NoError(err)

	s.waitForWSMessage(ctx, bConn, "system", "", 5*time.Second)
	bConn.Close()
	time.Sleep(500 * time.Millisecond)

	numMissed := 3
	var lastSeq int64
	aConn, _, err := s.dialWS(ctx, aTok, roomID)
	s.Require().NoError(err)
	defer aConn.Close()

	for i := 1; i <= numMissed; i++ {
		content := fmt.Sprintf("missed-msg-%d", i)
		aConn.WriteJSON(map[string]string{
			"type": "chat", "content": content, "client_msg_id": fmt.Sprintf("m-%d", i),
		})
		msg, _ := s.waitForWSMessage(ctx, aConn, "chat", content, 5*time.Second)
		lastSeq = int64(msg["sequence_number"].(float64))

		time.Sleep(600 * time.Millisecond)
	}

	time.Sleep(500 * time.Millisecond)

	syncURL := fmt.Sprintf("/rooms/%s/messages?last_seq=%d&limit=10", roomID, lastSeq-int64(numMissed))
	var syncRes struct {
		Messages []map[string]any `json:"messages"`
	}
	s.T().Logf("Bob requesting sync from seq %d", lastSeq-int64(numMissed))

	err = s.makeRequest(ctx, "GET", syncURL, nil, &syncRes, bTok)
	s.Require().NoError(err)

	s.Equal(numMissed, len(syncRes.Messages), "Exactly %d missed messages should be recovered via REST API", numMissed)
	s.Equal("missed-msg-1", syncRes.Messages[0]["content"])
	s.Equal("missed-msg-3", syncRes.Messages[numMissed-1]["content"])

	s.T().Log("Recovery Check PASSED: Missed messages successfully fetched from Chat Service")
}

func (s *E2ESuite) TestScenario_08_Room_Update() {
	s.T().Log(">>> starting TestScenario_08_Room_Update")
	ctx := s.T().Context()

	manager := s.generateUniqueUsername("updmgr")
	member := s.generateUniqueUsername("updmbr")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, manager, password))
	s.Require().NoError(s.signUp(ctx, member, password))

	mgrToken, _, err := s.login(ctx, manager, password)
	s.Require().NoError(err)
	mbrToken, _, err := s.login(ctx, member, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, mgrToken, "Before Update")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, mbrToken))

	updateBody := map[string]any{"name": "After Update", "capacity": 500}
	err = s.makeRequest(ctx, "PATCH", "/rooms/"+roomID, updateBody, nil, mgrToken)
	s.Require().NoError(err, "Manager should be able to update room")

	var listRes struct {
		Rooms []map[string]any `json:"rooms"`
	}
	err = s.makeRequest(ctx, "GET", "/me/rooms", nil, &listRes, mgrToken)
	s.Require().NoError(err)

	var found bool
	for _, r := range listRes.Rooms {
		if r["id"] == roomID {
			s.Equal("After Update", r["name"], "Room name should be updated")
			s.Equal(float64(500), r["capacity"], "Room capacity should be updated")
			found = true
			break
		}
	}
	s.Require().True(found, "Updated room should appear in room list")

	err = s.makeRequest(ctx, "PATCH", "/rooms/"+roomID, updateBody, nil, mbrToken)
	s.Require().Error(err, "Non-manager should not be able to update room")
	s.assertHTTPError(err, http.StatusForbidden)
}

func (s *E2ESuite) TestScenario_09_Room_LeaveAndBroadcast() {
	s.T().Log(">>> starting TestScenario_09_Room_LeaveAndBroadcast")
	ctx := s.T().Context()

	u1 := s.generateUniqueUsername("leave1")
	u2 := s.generateUniqueUsername("leave2")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, u1, password))
	s.Require().NoError(s.signUp(ctx, u2, password))

	t1, _, err := s.login(ctx, u1, password)
	s.Require().NoError(err)
	t2, _, err := s.login(ctx, u2, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, t1, "Leave Test Room")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, t2))

	u2Conn, _, err := s.dialWS(ctx, t2, roomID)
	s.Require().NoError(err)
	defer u2Conn.Close()

	err = s.makeRequest(ctx, "DELETE", "/rooms/"+roomID+"/members/me", nil, nil, t1)
	s.Require().NoError(err, "Leaving room must succeed")

	msg, err := s.waitForWSMessage(ctx, u2Conn, "system", "나갔습니다.", 5*time.Second)
	s.Require().NoError(err, "Remaining member should receive broadcast 'left' event")

	content := msg["content"].(string)
	s.Contains(content, "나갔습니다.", "System message should indicate user left event")
}

func (s *E2ESuite) TestScenario_10_RoomDeletion_ForceKick() {
	s.T().Log(">>> starting TestScenario_10_RoomDeletion_ForceKick")
	ctx := s.T().Context()

	time.Sleep(2 * time.Second)
	mgr := s.generateUniqueUsername("kickmgr")
	user1 := s.generateUniqueUsername("kicku1")
	user2 := s.generateUniqueUsername("kicku2")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, mgr, password))
	s.Require().NoError(s.signUp(ctx, user1, password))
	s.Require().NoError(s.signUp(ctx, user2, password))

	mgrTok, _, err := s.login(ctx, mgr, password)
	s.Require().NoError(err)
	u1Tok, _, err := s.login(ctx, user1, password)
	s.Require().NoError(err)
	u2Tok, _, err := s.login(ctx, user2, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, mgrTok, "Doomed Room")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, u1Tok))
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, u2Tok))

	mgrConn, _, err := s.dialWS(ctx, mgrTok, roomID)
	if err != nil {
		s.T().Logf("dialWS mgrConn err: %v", err)
	}
	u1Conn, _, err := s.dialWS(ctx, u1Tok, roomID)
	if err != nil {
		s.T().Logf("dialWS u1Conn err: %v", err)
	}
	u2Conn, _, err := s.dialWS(ctx, u2Tok, roomID)
	if err != nil {
		s.T().Logf("dialWS u2Conn err: %v", err)
	}

	if mgrConn != nil {
		defer mgrConn.Close()
	}
	if u1Conn != nil {
		defer u1Conn.Close()
	}
	if u2Conn != nil {
		defer u2Conn.Close()
	}

	time.Sleep(500 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(3)

	waitForClose := func(conn *websocket.Conn, name string) {
		defer wg.Done()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				s.T().Logf("[%s] WebSocket closed as expected: %v", name, err)
				return
			}
		}
	}

	if mgrConn != nil {
		go waitForClose(mgrConn, "Manager")
	} else {
		wg.Done()
	}
	if u1Conn != nil {
		go waitForClose(u1Conn, "User1")
	} else {
		wg.Done()
	}
	if u2Conn != nil {
		go waitForClose(u2Conn, "User2")
	} else {
		wg.Done()
	}

	s.T().Log("Manager deletes the room...")
	err = s.makeRequest(ctx, "DELETE", "/rooms/"+roomID, nil, nil, mgrTok)
	s.Require().NoError(err, "Room deletion API should succeed")

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.T().Log("Room Deletion & Force Kick PASSED: All websockets properly closed")
	case <-time.After(5 * time.Second):
		s.T().Fatal("Time out waiting for websockets to be closed after room deletion")
	}

	_, _, err = s.dialWS(ctx, u1Tok, roomID)
	s.Require().Error(err, "Should not be able to connect to a deleted room")
}

func (s *E2ESuite) TestScenario_11_RateLimit_MultiLevel() {
	s.T().Log(">>> starting TestScenario_11_RateLimit_MultiLevel")
	ctx := s.T().Context()

	user := s.generateUniqueUsername("rl")
	password := "SecurePass123!"

	var hitPublic bool
	for range 20 {
		err := s.signUp(ctx, user, "wrong")
		if err != nil && strings.Contains(err.Error(), "status 429") {
			hitPublic = true
			break
		}
	}
	s.Require().True(hitPublic, "Public Rate limit (IP based on /auth) should be triggered")

	time.Sleep(3 * time.Second)

	err := s.signUp(ctx, user, password)
	s.Require().NoError(err)
	token, _, err := s.login(ctx, user, password)
	s.Require().NoError(err)

	hitAuthenticated := false
	for range 40 {
		var res map[string]any
		err := s.makeRequest(ctx, "GET", "/me/rooms", nil, &res, token)
		if err != nil && strings.Contains(err.Error(), "status 429") {
			hitAuthenticated = true
			break
		}
	}
	s.Require().True(hitAuthenticated, "Authenticated Rate limit (UserID based on /rooms) should be triggered")

	time.Sleep(3 * time.Second)

	hitEstablish := false
	for range 20 {
		_, err := s.getWSTicket(ctx, token)
		if err != nil && strings.Contains(err.Error(), "status 429") {
			hitEstablish = true
			break
		}
	}
	s.Require().True(hitEstablish, "WS Establish Rate limit (UserID based on /ws/ticket) should be triggered")

	time.Sleep(3 * time.Second)
}

func (s *E2ESuite) TestScenario_12_RateLimit_SpamProtection() {
	s.T().Log(">>> starting TestScenario_12_RateLimit_SpamProtection")
	ctx := s.T().Context()

	time.Sleep(2 * time.Second)

	u1 := s.generateUniqueUsername("spam1")
	u2 := s.generateUniqueUsername("spam2")
	password := "SecurePass123!"

	s.Require().NoError(s.signUp(ctx, u1, password))
	time.Sleep(500 * time.Millisecond)
	s.Require().NoError(s.signUp(ctx, u2, password))
	time.Sleep(500 * time.Millisecond)

	t1, _, err := s.login(ctx, u1, password)
	s.Require().NoError(err)
	time.Sleep(500 * time.Millisecond)
	t2, _, err := s.login(ctx, u2, password)
	s.Require().NoError(err)

	roomID, err := s.createRoom(ctx, t1, "Spam Room")
	s.Require().NoError(err)
	s.Require().NoError(s.makeRequest(ctx, "PUT", "/rooms/"+roomID+"/members/me", nil, nil, t2))

	conn1, _, err := s.dialWS(ctx, t1, roomID)
	s.Require().NoError(err)
	defer conn1.Close()

	conn2, _, err := s.dialWS(ctx, t2, roomID)
	s.Require().NoError(err)
	defer conn2.Close()

	time.Sleep(500 * time.Millisecond)

	go func() {
		for i := range 30 {
			err := conn1.WriteJSON(map[string]string{
				"type":          "chat",
				"content":       fmt.Sprintf("spam-%d", i),
				"client_msg_id": fmt.Sprintf("cm-%d", i),
			})
			if err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	gotRateLimitMsg := false

	conn1.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, p, err := conn1.ReadMessage()
		if err != nil {
			s.T().Logf("[User1] ReadMessage loop ended: %v", err)
			break
		}
		var m map[string]any
		json.Unmarshal(p, &m)
		if m["type"] == "system" && m["sender_id"] == "system" {
			content, ok := m["content"].(string)
			if ok {
				s.T().Logf("[User1] Received system message: %s", content)
				if content == "rate limit exceeded: do not spam" {
					gotRateLimitMsg = true
					break
				}
			}
		}
	}
	s.Require().True(gotRateLimitMsg, "Spammer should receive WS_MESSAGE rate limit warning")

	conn2.SetReadDeadline(time.Now().Add(1 * time.Second))
	for {
		_, p, err := conn2.ReadMessage()
		if err != nil {
			break
		}
		var m map[string]any
		json.Unmarshal(p, &m)
		if content, ok := m["content"].(string); ok && content == "rate limit exceeded: do not spam" {
			s.T().Fatal("User2 should not receive User1's rate limit warning")
		}
	}
}
