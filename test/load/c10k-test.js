import exec from 'k6/execution';
import http from 'k6/http';
import ws from 'k6/ws';
import { sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const API_HOST = __ENV.API_HOST || 'api-gateway';
const WS_HOST = __ENV.WS_HOST || 'websocket-service';
const API_PORT = __ENV.API_PORT || '8080';
const WS_PORT = __ENV.WS_PORT || '8081';
const BASE_URL = `http://${API_HOST}:${API_PORT}`;
const WS_URL = `ws://${WS_HOST}:${WS_PORT}/ws`;

const PATHS = {
    HEALTH: '/health',
    ROOMS: '/rooms',
    SIGNUP: '/users',
    LOGIN: '/auth/token',
    WS_TICKET: '/auth/ws-ticket',
    MEMBERSHIP: (id) => `/rooms/${id}/members/me`,
    MESSAGES: (id) => `/rooms/${id}/messages`,
};

const VU_OFFSET = integerEnv('K6_VU_OFFSET', 0, 0);
const TARGET_VUS = integerEnv('K6_TARGET_VUS', 10000, 1);
const TOTAL_ROOMS = Math.max(1, Math.floor(TARGET_VUS / 100));
const MSG_INTERVAL = 5000;
const MSG_TIMEOUT = 10000;
const MSG_BODY = 'x'.repeat(128);
const WS_SESSION_DURATION = 60000;
const SYNC_CATCHUP_MAX_PAGES = 5;

const msgLatency = new Trend('msg_latency', true);
const msgLatencySamples = new Counter('msg_latency_samples');
const historyFetchDuration = new Trend('history_fetch_duration', true);
const syncFetchDuration = new Trend('sync_fetch_duration', true);

const authErrors = new Counter('auth_errors');
const joinErrors = new Counter('join_errors');
const ticketErrors = new Counter('ticket_errors');
const wsConnectErrors = new Counter('ws_connect_errors');
const msgTimeouts = new Counter('msg_timeouts');
const msgSent = new Counter('msg_sent');
const msgPublishErrors = new Counter('msg_publish_errors');

const wsOpens = new Counter('ws_opens');
const wsCloses = new Counter('ws_closes');

export const options = {
    systemTags: [
        'proto', 'subproto', 'status', 'method', 'name', 'group', 'check',
        'error', 'error_code', 'tls_version', 'scenario', 'service', 'expected_response',
    ],
    scenarios: {
        c10k_challenge: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: __ENV.K6_RAMP_DURATION || '10m', target: TARGET_VUS },
                { duration: __ENV.K6_PLATEAU_DURATION || '3m', target: TARGET_VUS },
                { duration: __ENV.K6_RAMP_DOWN_DURATION || '2m', target: 0 },
            ],
            gracefulRampDown: '1m',
            gracefulStop: '1m',
        },
    },
    summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
    thresholds: {
        msg_latency: ['p(99)<50'],
        msg_latency_samples: ['count>0'],
        history_fetch_duration: ['p(99)<100'],
        sync_fetch_duration: ['p(99)<100'],
        auth_errors: ['count<1'],
        join_errors: ['count<1'],
        ticket_errors: ['count<1'],
        ws_connect_errors: ['count<1'],
        msg_timeouts: ['count<1'],
        msg_publish_errors: ['count<1'],
    },
};

export function setup() {
    const runId = crypto.randomUUID().slice(0, 6);
    console.log(`[setup] run=${runId} offset=${VU_OFFSET} vus=${TARGET_VUS} rooms=${TOTAL_ROOMS}`);

    const healthRes = http.get(`${BASE_URL}${PATHS.HEALTH}`, { timeout: '3s', responseType: 'none' });
    if (healthRes.status !== 200) throw new Error(`서비스 미준비: ${healthRes.status}`);

    const adminUser = `a${runId}${VU_OFFSET}`;
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

    const authHeader = { ...jsonHeader, Authorization: `Bearer ${token}` };
    const rooms = [];

    for (let i = 0; i < TOTAL_ROOMS; i++) {
        const roomName = `r${runId}${VU_OFFSET}${i}`;
        const res = http.post(
            `${BASE_URL}${PATHS.ROOMS}`,
            JSON.stringify({ name: roomName, capacity: 1000 }),
            { headers: authHeader },
        );

        if (res.status === 201) {
            rooms[i] = res.json('room_id');
        } else if (res.status === 409) {
            const listRes = http.get(`${BASE_URL}${PATHS.ROOMS}?q=${roomName}&limit=100`, {
                headers: authHeader,
                tags: { name: 'GET /rooms' },
            });
            const found = listRes.json('rooms').find(r => r.name === roomName);
            if (found) rooms[i] = found.id;
            else throw new Error(`방 목록에서 ${roomName} 못 찾음`);
        } else {
            throw new Error(`방 생성 실패 ${roomName}: ${res.status}`);
        }
        sleep(0.1);
    }

    return { runId, rooms };
}

const session = {
    token: null,
    roomId: null,
    username: null,
    fakeIp: null,
    lastId: null,
};

export default function c10kChallenge({ runId, rooms }) {
    const globalVu = exec.vu.idInTest + VU_OFFSET;
    const role = resolveRole(globalVu);

    const fakeIp = `10.0.${Math.floor(globalVu / 256)}.${globalVu % 256}`;
    session.fakeIp = fakeIp;
    if (!authenticate(runId, globalVu, fakeIp)) {
        sleep(5);
        return;
    }

    const authHeader = authHeaders();

    if (!session.roomId) {
        session.roomId = rooms[resolveRoomIndex(globalVu, role)];
    }

    try {
        retryWithBackoff(() => {
            const res = http.put(`${BASE_URL}${PATHS.MEMBERSHIP(session.roomId)}`, null, {
                headers: authHeader,
                responseType: 'none',
                tags: { name: 'PUT /rooms/:id/members/me' },
            });
            return { success: res.status === 200 || res.status === 204 || res.status === 409, res };
        }, 'JoinRoom');
    } catch {
        joinErrors.add(1);
        sleep(5);
        return;
    }

    fetchMessages();

    const ticket = acquireTicket();
    if (!ticket) {
        sleep(5);
        return;
    }

    chatOverWebSocket(ticket, role, fakeIp);

    if (role.isChurner && TOTAL_ROOMS > 1) {
        http.del(`${BASE_URL}${PATHS.MEMBERSHIP(session.roomId)}`, null, {
            headers: authHeader,
            responseType: 'none',
            tags: { name: 'DELETE /rooms/:id/members/me' },
        });
        session.roomId = null;
        session.lastId = null;
    }

    sleep(1);
}

function integerEnv(name, fallback, minimum) {
    const value = Number(__ENV[name] ?? fallback);
    if (!Number.isSafeInteger(value) || value < minimum) {
        throw new Error(`${name} must be an integer >= ${minimum}`);
    }
    return value;
}

function retryWithBackoff(request, label, maxAttempts = 3) {
    for (let attempt = 0; attempt < maxAttempts; attempt++) {
        try {
            const result = request();
            if (result.success) return result.res;
            if (result.res?.status === 401) throw new Error('Unauthorized');
        } catch (e) {
            if (e.message === 'Unauthorized') throw e;
        }
        if (attempt + 1 >= maxAttempts) throw new Error(`${label} failed after ${maxAttempts} attempts`);
        sleep((2 ** (attempt + 1) * 100 + Math.random() * 50) / 1000);
    }
}

function resolveRole(globalVu) {
    let bucket = globalVu % 100;
    if (TARGET_VUS >= 100) {
        const localIndex = globalVu - VU_OFFSET - 1;
        const roomIndex = localIndex % TOTAL_ROOMS;
        const memberIndex = Math.floor(localIndex / TOTAL_ROOMS);
        // 방별 100명 중 probe 1명, churner 2명, reconnector 7명을 선정한다.
        bucket = memberIndex < 100 ? (memberIndex + roomIndex) % 100 : 99;
    }
    const isChurner = bucket >= 1 && bucket < 3;
    const isReconnector = bucket >= 3 && bucket < 10;
    return {
        isProbe: bucket === 0,
        isChurner,
        isReconnector,
        isTransient: isChurner || isReconnector,
    };
}

function resolveRoomIndex(globalVu, role) {
    const localVu = globalVu - VU_OFFSET;
    const roomOffset = role.isChurner ? exec.vu.iterationInScenario + 1 : 0;
    return (localVu - 1 + roomOffset) % TOTAL_ROOMS;
}

function withForwardedFor(headers, fakeIp) {
    return fakeIp ? { ...headers, 'X-Forwarded-For': fakeIp } : headers;
}

function authHeaders(extra = {}) {
    return withForwardedFor({ ...extra, Authorization: `Bearer ${session.token}` }, session.fakeIp);
}

function authenticate(runId, globalVu, fakeIp) {
    if (session.token) return true;
    session.username = `u${runId}${globalVu}`;
    const body = JSON.stringify({ username: session.username, password: 'Password123!' });
    const headers = withForwardedFor({ 'Content-Type': 'application/json' }, fakeIp);
    try {
        retryWithBackoff(() => {
            const res = http.post(`${BASE_URL}${PATHS.SIGNUP}`, body, { headers, responseType: 'none' });
            return { success: res.status === 201 || res.status === 409, res };
        }, 'Signup');
        const loginRes = retryWithBackoff(() => {
            const res = http.post(`${BASE_URL}${PATHS.LOGIN}`, body, { headers });
            return { success: res.status === 200, res };
        }, 'Login');
        session.token = loginRes.json('access_token');
        if (!session.token) throw new Error('로그인 응답에 토큰 없음');
        return true;
    } catch {
        authErrors.add(1);
        return false;
    }
}

function maxMessageId(messages) {
    if (!messages || messages.length === 0) return '';
    return messages.reduce((max, m) => ((m.id || '') > max ? m.id : max), '');
}

function fetchMessages() {
    const syncing = session.lastId !== null && session.lastId !== '';
    const metric = syncing ? syncFetchDuration : historyFetchDuration;

    let cursor = syncing ? session.lastId : null;
    const maxPages = syncing ? SYNC_CATCHUP_MAX_PAGES : 1;

    for (let page = 0; page < maxPages; page++) {
        const query = cursor ? `?after_id=${cursor}&limit=50` : '?limit=50';
        try {
            const res = http.get(
                `${BASE_URL}${PATHS.MESSAGES(session.roomId)}${query}`,
                { headers: authHeaders(), tags: { name: 'GET /rooms/:id/messages' } },
            );
            if (res.status !== 200) return;

            metric.add(res.timings.duration);
            const messages = res.json('messages');
            const maxId = maxMessageId(messages);
            if (maxId > (session.lastId || '')) session.lastId = maxId;
            if (session.lastId === null) session.lastId = '';

            if (!res.json('has_more') || maxId === '') return;
            cursor = maxId;
        } catch {
            return;
        }
    }
}

function acquireTicket() {
    try {
        const ticketRes = retryWithBackoff(() => {
            const res = http.post(`${BASE_URL}${PATHS.WS_TICKET}`, null, {
                headers: authHeaders(),
            });
            return { success: res.status === 200, res };
        }, 'WSTicket');
        const ticket = ticketRes.json('ticket');
        if (!ticket) throw new Error('WebSocket 티켓 없음');
        return ticket;
    } catch {
        ticketErrors.add(1);
        return null;
    }
}

function sendChatMessage(socket, pendingProbes) {
    const clientMsgId = crypto.randomUUID();
    msgSent.add(1);
    if (pendingProbes) pendingProbes.set(clientMsgId, Date.now());
    const payload = JSON.stringify({ type: 'chat', content: MSG_BODY, client_msg_id: clientMsgId });

    try {
        socket.send(payload);
    } catch {
        msgPublishErrors.add(1);
        pendingProbes?.delete(clientMsgId);
    }
}

function expirePendingMessages(pending) {
    const now = Date.now();
    for (const [id, sentAt] of pending) {
        if (now - sentAt > MSG_TIMEOUT) {
            msgTimeouts.add(1);
            pending.delete(id);
        }
    }
}

function receiveChatMessage(raw, role, pendingProbes) {
    let msg;
    try {
        msg = JSON.parse(raw);
    } catch {
        return;
    }
    if (role.isReconnector && msg.id && msg.id > (session.lastId || '')) {
        session.lastId = msg.id;
    }
    if (!role.isProbe || !msg.client_msg_id) return;
    const sentAt = pendingProbes.get(msg.client_msg_id);
    if (sentAt !== undefined) {
        msgLatency.add(Date.now() - sentAt);
        msgLatencySamples.add(1);
        pendingProbes.delete(msg.client_msg_id);
    }
}

function chatOverWebSocket(ticket, role, fakeIp) {
    const connUrl = `${WS_URL}?ticket=${ticket}&room_id=${session.roomId}`;
    const pendingProbes = role.isProbe ? new Map() : null;
    let intentionalClose = false;
    const connRes = ws.connect(connUrl, {
        headers: { 'X-Forwarded-For': fakeIp },
        tags: { name: 'WS /ws' },
    }, (socket) => {
        socket.on('open', () => {
            wsOpens.add(1);
            if (role.isTransient) {
                socket.setTimeout(() => {
                    intentionalClose = true;
                    socket.close();
                }, WS_SESSION_DURATION);
            }
            socket.setInterval(() => sendChatMessage(socket, pendingProbes), MSG_INTERVAL);
            if (role.isProbe) {
                socket.setInterval(() => expirePendingMessages(pendingProbes), MSG_TIMEOUT);
            }
        });

        if (role.isProbe || role.isReconnector) {
            socket.on('message', (raw) => receiveChatMessage(raw, role, pendingProbes));
        }

        socket.on('close', (code) => {
            wsCloses.add(1);
            const normalClose = intentionalClose || code === 1000 || code === 1001;
            if (role.isProbe && !normalClose) msgTimeouts.add(pendingProbes.size);
            pendingProbes?.clear();
        });
    });
    if (connRes.status !== 101) wsConnectErrors.add(1);
}

export function handleSummary(data) {
    return { stdout: `summary:${JSON.stringify(data)}\n` };
}
