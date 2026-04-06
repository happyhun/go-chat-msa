import http from 'k6/http';
import ws from 'k6/ws';
import { sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';

// 서버 접속 정보
const API_HOST = __ENV.API_HOST || 'api-gateway';
const WS_HOST = __ENV.WS_HOST || 'ws-gateway';
const API_PORT = __ENV.API_PORT || '8080';
const WS_PORT = __ENV.WS_PORT || '8088';
const BASE_URL = `http://${API_HOST}:${API_PORT}`;
const WS_URL = `ws://${WS_HOST}:${WS_PORT}/ws`;

// API 경로
const PATHS = {
    HEALTH: '/health',
    ROOMS: '/rooms',
    SIGNUP: '/users',
    LOGIN: '/auth/token',
    MEMBERSHIP: (id) => `/rooms/${id}/members/me`,
    MESSAGES: (id) => `/rooms/${id}/messages`,
};

// 테스트 실행 파라미터
const RUN_ID = Math.random().toString(36).substring(2, 5);
const VU_OFFSET = __ENV.K6_VU_OFFSET ? parseInt(__ENV.K6_VU_OFFSET, 10) : 0;
const TARGET_VUS = __ENV.K6_TARGET_VUS ? parseInt(__ENV.K6_TARGET_VUS, 10) : 10000;
const TOTAL_ROOMS = Math.max(1, Math.floor(TARGET_VUS / 100)); // 방당 100명
const MSG_INTERVAL = 5000;
const MSG_TIMEOUT = 10000;
const MSG_BODY = 'x'.repeat(128);
const WS_SESSION_DURATION = 60000;

// 커스텀 메트릭 - 지연 시간
const msgLatency = new Trend('msg_latency', true);
const historyFetchDuration = new Trend('history_fetch_duration', true);
const syncFetchDuration = new Trend('sync_fetch_duration', true);

// 커스텀 메트릭 - 에러 카운터
const authErrors = new Counter('auth_errors');
const joinErrors = new Counter('join_errors');
const ticketErrors = new Counter('ticket_errors');
const wsConnectErrors = new Counter('ws_connect_errors');
const msgTimeouts = new Counter('msg_timeouts');

// 커스텀 메트릭 - WS 연결 추이
const wsOpens = new Counter('ws_opens');
const wsCloses = new Counter('ws_closes');

// k6 옵션 설정
export const options = {
    scenarios: {
        c10k_challenge: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: '10m', target: TARGET_VUS },
                { duration: '3m', target: TARGET_VUS },
                { duration: '2m', target: 0 },
            ],
            gracefulRampDown: '1m',
            gracefulStop: '1m',
        },
    },
    summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
    thresholds: {
        'msg_latency': ['p(99)<50'],
        'history_fetch_duration': ['p(99)<100'],
        'sync_fetch_duration': ['p(99)<100'],
        'ws_connect_errors': ['count<1'],
        'msg_timeouts': ['count<1'],
    },
};

// admin 계정 생성 및 테스트용 방 사전 생성
export function setup() {
    console.log(`[setup] run=${RUN_ID} offset=${VU_OFFSET} vus=${TARGET_VUS} rooms=${TOTAL_ROOMS}`);

    const healthRes = http.get(`${BASE_URL}${PATHS.HEALTH}`, { timeout: '3s' });
    if (healthRes.status !== 200) throw new Error(`서비스 미준비: ${healthRes.status}`);

    const adminUser = `a${RUN_ID}${VU_OFFSET}`;
    const adminBody = JSON.stringify({ username: adminUser, password: 'AdminPass123!' });
    const jsonHeader = { 'Content-Type': 'application/json' };

    const signupRes = http.post(`${BASE_URL}${PATHS.SIGNUP}`, adminBody, { headers: jsonHeader });
    if (signupRes.status !== 201 && signupRes.status !== 409) {
        throw new Error(`admin 가입 실패: ${signupRes.status} ${signupRes.body}`);
    }

    const loginRes = http.post(`${BASE_URL}${PATHS.LOGIN}`, adminBody, { headers: jsonHeader });
    if (loginRes.status !== 200) {
        throw new Error(`admin 로그인 실패: ${loginRes.status} ${loginRes.body}`);
    }

    const token = loginRes.json('access_token');
    if (!token) throw new Error('admin 토큰 없음');

    const authHeader = { ...jsonHeader, 'Authorization': `Bearer ${token}` };
    const rooms = {};

    for (let i = 0; i < TOTAL_ROOMS; i++) {
        const roomName = `r${RUN_ID}${VU_OFFSET}${i}`;
        const res = http.post(
            `${BASE_URL}${PATHS.ROOMS}`,
            JSON.stringify({ name: roomName, capacity: 1000 }),
            { headers: authHeader },
        );

        if (res.status === 201) {
            rooms[i] = res.json('room_id');
        } else if (res.status === 409) {
            const listRes = http.get(`${BASE_URL}${PATHS.ROOMS}?q=${roomName}&limit=100`, { headers: authHeader });
            const found = listRes.json('rooms').find(r => r.name === roomName);
            if (found) rooms[i] = found.id;
            else throw new Error(`방 목록에서 ${roomName} 못 찾음`);
        } else {
            throw new Error(`방 생성 실패 ${roomName}: ${res.status}`);
        }
        sleep(0.1); // rate limit 회피
    }

    return { rooms };
}

// VU별 세션 상태
let session = {
    token: null, roomId: null, username: null,
    msgCount: 0, lastSeq: null, iteration: 0,
};

// 메인 시나리오
export default function (data) {
    const globalVu = __VU + VU_OFFSET;
    const role = resolveRole(globalVu);
    session.iteration++;

    // 1. 인증
    const fakeIp = `10.0.${Math.floor(globalVu / 256)}.${globalVu % 256}`;
    if (!authenticate(globalVu, fakeIp)) { sleep(5); return; }

    const authHeader = { 'Authorization': `Bearer ${session.token}` };

    // 2. 방 배정 (Churner는 매 iteration마다 다른 방)
    if (!session.roomId) {
        const roomIdx = role.isChurner
            ? ((__VU - 1 + session.iteration) % TOTAL_ROOMS)
            : ((__VU - 1) % TOTAL_ROOMS);
        session.roomId = data.rooms[roomIdx];
    }

    // 3. 방 입장
    try {
        retryWithBackoff(() => {
            const res = http.put(`${BASE_URL}${PATHS.MEMBERSHIP(session.roomId)}`, null, {
                headers: authHeader,
            });
            return { success: res.status === 200 || res.status === 204 || res.status === 409, res };
        }, 'JoinRoom');
    } catch (e) {
        joinErrors.add(1);
        sleep(5); return;
    }

    // 4. 채팅 히스토리 조회
    fetchMessages();

    // 5. WS 티켓 발급 및 채팅 시작
    const ticket = acquireTicket();
    if (!ticket) { sleep(5); return; }

    chatOverWebSocket(ticket, role, fakeIp);

    // 6. Churner 퇴장 → 다음 iteration에서 새 방으로
    if (role.isChurner && TOTAL_ROOMS > 1) {
        http.del(`${BASE_URL}${PATHS.MEMBERSHIP(session.roomId)}`, null, {
            headers: authHeader,
        });
        session.roomId = null;
        session.lastSeq = null;
    }

    sleep(1);
}

function retryWithBackoff(fn, label, maxRetries = 3) {
    for (let attempt = 0; attempt < maxRetries; attempt++) {
        try {
            const result = fn();
            if (result.success) return result.res;
            if (result.res?.status === 401) throw new Error('Unauthorized');
        } catch (e) {
            if (e.message === 'Unauthorized') throw e;
        }
        if (attempt + 1 >= maxRetries) throw new Error(`${label} failed after ${maxRetries} attempts`);
        sleep((Math.pow(2, attempt + 1) * 100 + Math.random() * 50) / 1000);
    }
}

// VU 역할 분배
// Probe(1%): 장기 접속 + 레이턴시 샘플링
// Churner(2%): 방 이탈 후 다른 방 입장 반복
// Reconnector(7%): 재접속하며 누락 메시지 동기화
// Stalker(90%): 장기 접속 유지
function resolveRole(globalVu) {
    const bucket = globalVu % 100;
    const isChurner = bucket >= 1 && bucket < 3;
    const isReconnector = bucket >= 3 && bucket < 10;
    return {
        isProbe: bucket === 0,
        isChurner,
        isReconnector,
        isTransient: isChurner || isReconnector,
    };
}

function authenticate(globalVu, fakeIp) {
    if (session.token) return true;
    session.username = `u${RUN_ID}${globalVu}`;
    const body = JSON.stringify({ username: session.username, password: 'Password123!' });
    const headers = { 'Content-Type': 'application/json', 'X-Forwarded-For': fakeIp };
    try {
        retryWithBackoff(() => {
            const res = http.post(`${BASE_URL}${PATHS.SIGNUP}`, body, { headers });
            return { success: res.status === 201 || res.status === 409, res };
        }, 'Signup');
        const loginRes = retryWithBackoff(() => {
            const res = http.post(`${BASE_URL}${PATHS.LOGIN}`, body, { headers });
            return { success: res.status === 200, res };
        }, 'Login');
        session.token = loginRes.json('access_token');
        return true;
    } catch (e) {
        authErrors.add(1);
        return false;
    }
}

function fetchMessages() {
    const syncing = session.lastSeq !== null;
    const query = syncing
        ? `?last_seq=${session.lastSeq}&limit=50`
        : '?limit=50';
    const metric = syncing ? syncFetchDuration : historyFetchDuration;
    try {
        const res = http.get(
            `${BASE_URL}${PATHS.MESSAGES(session.roomId)}${query}`,
            { headers: { 'Authorization': `Bearer ${session.token}` } },
        );
        if (res.status === 200) {
            metric.add(res.timings.duration);
            const messages = res.json('messages');
            const maxSeq = (messages && messages.length > 0)
                ? messages.reduce((max, m) => Math.max(max, m.sequence_number || 0), 0)
                : 0;
            session.lastSeq = syncing
                ? Math.max(session.lastSeq, maxSeq)
                : maxSeq;
        }
    } catch (_) { }
}

function acquireTicket() {
    try {
        const ticketRes = retryWithBackoff(() => {
            const res = http.post(`http://${WS_HOST}:${WS_PORT}/ws/ticket`, null, {
                headers: { 'Authorization': `Bearer ${session.token}` },
            });
            return { success: res.status === 200, res };
        }, 'WSTicket');
        return ticketRes.json('ticket');
    } catch (e) {
        ticketErrors.add(1);
        return null;
    }
}

function chatOverWebSocket(ticket, role, fakeIp) {
    const connUrl = `${WS_URL}?ticket=${ticket}&room_id=${session.roomId}`;
    const pendingProbes = new Map();
    const connRes = ws.connect(connUrl, { headers: { 'X-Forwarded-For': fakeIp } }, function (socket) {
        socket.on('open', function () {
            wsOpens.add(1);
            if (role.isTransient) {
                socket.setTimeout(function () { socket.close(); }, WS_SESSION_DURATION);
            }
            socket.setInterval(function () {
                session.msgCount++;
                const clientMsgId = `${RUN_ID}-${__VU + VU_OFFSET}-${session.msgCount}`;
                socket.send(`{"type":"chat","content":"${MSG_BODY}","client_msg_id":"${clientMsgId}"}`);
                if (role.isProbe) pendingProbes.set(clientMsgId, Date.now());
            }, MSG_INTERVAL);
            if (role.isProbe) {
                socket.setInterval(function () {
                    const now = Date.now();
                    for (const [id, sentAt] of pendingProbes) {
                        if (now - sentAt > MSG_TIMEOUT) {
                            msgTimeouts.add(1);
                            pendingProbes.delete(id);
                        }
                    }
                }, MSG_TIMEOUT);
            }
        });
        if (role.isReconnector) socket.on('message', function (raw) {
            try {
                const msg = JSON.parse(raw);
                if (msg.sequence_number) {
                    session.lastSeq = Math.max(session.lastSeq, msg.sequence_number);
                }
            } catch (_) { }
        });
        if (role.isProbe) socket.on('message', function (raw) {
            try {
                const msg = JSON.parse(raw);
                if (msg.client_msg_id) {
                    const sentAt = pendingProbes.get(msg.client_msg_id);
                    if (sentAt) {
                        msgLatency.add(Date.now() - sentAt);
                        pendingProbes.delete(msg.client_msg_id);
                    }
                }
            } catch (_) { }
        });
        socket.on('close', function () { wsCloses.add(1); });
    });
    if (connRes.status !== 101) wsConnectErrors.add(1);
}
