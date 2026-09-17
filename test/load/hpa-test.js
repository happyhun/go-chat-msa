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

const RUN_ID = Math.random().toString(36).substring(2, 6);
const TARGET_VUS = 300;
const TOTAL_ROOMS = 30;
const ROOM_CAPACITY = 100;
const MSG_INTERVAL = 5000;
const MSG_TIMEOUT = 10000;
const MSG_BODY = 'hpa-check';
const WS_SESSION_DURATION = 90 * 1000;
const WS_CONNECT_RETRIES = 3;
const WS_CONNECT_RETRY_MIN_MS = 250;
const WS_CONNECT_RETRY_MAX_MS = 1500;
const SYNC_LIMIT = 1000;
const SYNC_CATCHUP_MAX_PAGES = 5;
const PERSIST_SETTLE_SECONDS = 5;
const SYNC_REWIND_MS = 2000;
const UUID_V7_PATTERN = /^([0-9a-f]{8})-([0-9a-f]{4})-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

const msgLatency = new Trend('msg_latency', true);
const historyFetchDuration = new Trend('history_fetch_duration', true);
const syncFetchDuration = new Trend('sync_fetch_duration', true);

const authErrors = new Counter('auth_errors');
const joinErrors = new Counter('join_errors');
const ticketErrors = new Counter('ticket_errors');
const wsConnectErrors = new Counter('ws_connect_errors');
const wsConnectRetries = new Counter('ws_connect_retries');
const wsUnplannedCloses = new Counter('ws_unplanned_closes');
const wsReconnects = new Counter('ws_reconnects');
const reconnectRecovered = new Counter('reconnect_recovered');
const msgTimeouts = new Counter('msg_timeouts');
const msgPublishErrors = new Counter('msg_publish_errors');
const msgAttempts = new Counter('msg_attempts');
const acceptedReceipts = new Counter('accepted_receipts');
const acceptedMissing = new Counter('accepted_missing');
const syncErrors = new Counter('sync_errors');

const frameGaps = new Counter('frame_gaps');
const unresolvedObserved = new Counter('unresolved_observed');
const liveMissed = new Counter('live_missed');
const finalMissing = new Counter('final_missing');
const duplicateDelivered = new Counter('duplicate_delivered');

function clientMessageUUID() {
    return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (character) => {
        const random = Math.floor(Math.random() * 16);
        return (character === 'x' ? random : (random & 3) | 8).toString(16);
    });
}

function rewindMessageId(id, rewindMs = SYNC_REWIND_MS) {
    const match = UUID_V7_PATTERN.exec(id);
    if (!match) return id;

    const timestamp = Number.parseInt(match[1] + match[2], 16);
    const rewound = Math.max(0, timestamp - rewindMs).toString(16).padStart(12, '0');
    return `${rewound.slice(0, 8)}-${rewound.slice(8)}-7000-8000-000000000000`;
}

export const options = {
    scenarios: {
        hpa_handoff_consistency: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: '1m', target: 150 },
                { duration: '1m', target: TARGET_VUS },
                { duration: '2m', target: TARGET_VUS },
                { duration: '1m', target: 0 },
            ],
            gracefulRampDown: '2m',
            gracefulStop: '2m',
        },
    },
    summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
    thresholds: {
        auth_errors: ['count<1'],
        join_errors: ['count<1'],
        ticket_errors: ['count<1'],
        ws_connect_errors: ['count<1'],
        msg_timeouts: ['count<1'],
        msg_publish_errors: ['count<1'],
        msg_attempts: ['count>0'],
        accepted_receipts: ['count>0'],
        accepted_missing: ['count<1'],
        sync_errors: ['count<1'],
        final_missing: ['count<1'],
        duplicate_delivered: ['count<1'],
    },
};

export function setup() {
    console.log(`[setup] hpa run=${RUN_ID} vus=${TARGET_VUS} rooms=${TOTAL_ROOMS}`);

    const healthRes = http.get(`${BASE_URL}${PATHS.HEALTH}`, { timeout: '3s' });
    if (healthRes.status !== 200) throw new Error(`서비스 미준비: ${healthRes.status}`);

    const adminUser = `ha${RUN_ID}`;
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
    const rooms = {};

    for (let i = 0; i < TOTAL_ROOMS; i++) {
        const roomName = `hpa-${RUN_ID}-${i}`;
        const res = http.post(
            `${BASE_URL}${PATHS.ROOMS}`,
            JSON.stringify({ name: roomName, capacity: ROOM_CAPACITY }),
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
            throw new Error(`방 생성 실패 ${roomName}: ${res.status} ${res.body}`);
        }
        sleep(0.05);
    }

    return { rooms };
}

let session = {
    token: null,
    roomId: null,
    username: null,
    fakeIp: null,
    msgCount: 0,
    lastId: null,
    unresolvedIds: [],
};

export default function (data) {
    const globalVu = __VU;
    session.fakeIp = `10.1.${Math.floor(globalVu / 256)}.${globalVu % 256}`;

    if (!authenticate(globalVu)) {
        sleep(5);
        return;
    }

    if (!session.roomId) {
        const roomIdx = (globalVu - 1) % TOTAL_ROOMS;
        session.roomId = data.rooms[roomIdx];
    }

    try {
        retryWithBackoff(() => {
            const res = http.put(`${BASE_URL}${PATHS.MEMBERSHIP(session.roomId)}`, null, {
                headers: authHeaders(),
            });
            return { success: res.status === 200 || res.status === 204 || res.status === 409, res };
        }, 'JoinRoom');
    } catch (_) {
        joinErrors.add(1);
        sleep(5);
        return;
    }

    const receivedIds = new Set();
    const acceptedIds = new Set();
    if (session.lastId === null) fetchInitialMessages(receivedIds);
    const sessionStartCursor = session.lastId;

    const sessionEndCursor = chatOverWebSocket(sessionStartCursor, receivedIds, acceptedIds);
    verifyAccepted(sessionStartCursor, acceptedIds);
    if (!sessionEndCursor) {
        sleep(1);
        return;
    }

    sleep(PERSIST_SETTLE_SECONDS);
    session.unresolvedIds = observeMissed(sessionStartCursor, sessionEndCursor, receivedIds);
    unresolvedObserved.add(session.unresolvedIds.length);
    if (session.unresolvedIds.length > 0) {
        fetchAtSessionStart(rewindMessageId(sessionStartCursor || ''), receivedIds);
        resolveUnresolved(receivedIds);
    }

    sleep(1);
}

function authenticate(globalVu) {
    if (session.token) return true;

    session.username = `h${RUN_ID}${globalVu}`;
    const body = JSON.stringify({ username: session.username, password: 'Password123!' });
    const headers = withForwardedFor({ 'Content-Type': 'application/json' });

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
        session.senderId = loginRes.json('user_id');
        return true;
    } catch (_) {
        authErrors.add(1);
        return false;
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
        return ticketRes.json('ticket');
    } catch (_) {
        ticketErrors.add(1);
        return null;
    }
}

function chatOverWebSocket(sessionStartCursor, receivedIds, acceptedIds) {
    for (let attempt = 0; attempt <= WS_CONNECT_RETRIES; attempt++) {
        const ticket = acquireTicket();
        if (!ticket) {
            sleep(5);
            return false;
        }

        const result = connectWebSocket(ticket, sessionStartCursor, receivedIds, acceptedIds);
        if (result.status === 101) return result.endCursor;
        if (result.status === 503 && attempt < WS_CONNECT_RETRIES) {
            wsConnectRetries.add(1);
            sleep(randomBetween(WS_CONNECT_RETRY_MIN_MS, WS_CONNECT_RETRY_MAX_MS) / 1000);
            continue;
        }

        wsConnectErrors.add(1);
        return false;
    }
    return null;
}

function connectWebSocket(ticket, sessionStartCursor, receivedIds, acceptedIds) {
    const connUrl = `${WS_URL}?ticket=${ticket}&room_id=${session.roomId}`;
    const pending = new Map();
    const seenClientMsgIds = new Set();
    let lastFrameNo = 0;
    let intentionalClose = false;

    const connRes = ws.connect(connUrl, { headers: withForwardedFor({}) }, function (socket) {
        socket.on('open', function () {
            const stopSendingAt = Date.now() + WS_SESSION_DURATION - MSG_TIMEOUT;
            session.connections = (session.connections || 0) + 1;
            const receivedBeforeSync = receivedIds.size;
            fetchAtSessionStart(sessionStartCursor, receivedIds);
            if (session.connections > 1) {
                wsReconnects.add(1);
                reconnectRecovered.add(receivedIds.size - receivedBeforeSync);
            }
            socket.setTimeout(function () {
                fetchAtSessionStart(rewindMessageId(sessionStartCursor || ''), receivedIds);
                resolveUnresolved(receivedIds);
            }, SYNC_REWIND_MS);
            socket.setTimeout(function () {
                intentionalClose = true;
                socket.close();
            }, WS_SESSION_DURATION);
            socket.setInterval(function () {
                if (Date.now() >= stopSendingAt) return;
                session.msgCount++;
                const clientMsgId = clientMessageUUID();
                pending.set(clientMsgId, Date.now());
                msgAttempts.add(1);
                console.log(`attempt:${JSON.stringify({ room_id: session.roomId, sender_id: session.senderId, client_msg_id: clientMsgId })}`);
                try {
                    socket.send(JSON.stringify({ type: 'chat', content: MSG_BODY, client_msg_id: clientMsgId }));
                } catch (_) {
                    msgPublishErrors.add(1);
                    pending.delete(clientMsgId);
                }
            }, MSG_INTERVAL);
            socket.setInterval(function () {
                const now = Date.now();
                for (const [id, sentAt] of pending) {
                    if (now - sentAt > MSG_TIMEOUT) {
                        msgTimeouts.add(1);
                        pending.delete(id);
                    }
                }
            }, MSG_TIMEOUT);
        });

        socket.on('message', function (raw) {
            try {
                const msg = JSON.parse(raw);
                if (typeof msg.frame_no === 'number') {
                    if (lastFrameNo > 0 && msg.frame_no > lastFrameNo + 1) {
                        frameGaps.add(msg.frame_no - lastFrameNo - 1);
                    }
                    if (msg.frame_no > lastFrameNo) lastFrameNo = msg.frame_no;
                }
                if (msg.id) {
                    receivedIds.add(msg.id);
                    if (msg.id > (session.lastId || '')) session.lastId = msg.id;
                }
                if (msg.client_msg_id) {
                    const key = `${msg.room_id}:${msg.sender_id}:${msg.client_msg_id}`;
                    if (seenClientMsgIds.has(key)) {
                        duplicateDelivered.add(1);
                    } else {
                        seenClientMsgIds.add(key);
                    }
                    const sentAt = pending.get(msg.client_msg_id);
                    if (sentAt) {
                        msgLatency.add(Date.now() - sentAt);
                        pending.delete(msg.client_msg_id);
                        acceptedIds.add(msg.id);
                        acceptedReceipts.add(1);
                        console.log(`receipt:${JSON.stringify({ id: msg.id, room_id: session.roomId, client_msg_id: msg.client_msg_id })}`);
                    }
                }
            } catch (_) { }
        });
        socket.on('close', function () {
            if (!intentionalClose) wsUnplannedCloses.add(1);
            msgTimeouts.add(pending.size);
            pending.clear();
        });
    });

    return { status: connRes.status, endCursor: session.lastId };
}

function fetchInitialMessages(receivedIds) {
    const result = requestMessages(null, 50, historyFetchDuration);
    if (!result.ok) return;
    for (const id of result.ids) receivedIds.add(id);
    session.lastId = result.maxId;
}

function fetchAtSessionStart(fromId, receivedIds) {
    let cursor = fromId || '';
    for (let page = 0; page < SYNC_CATCHUP_MAX_PAGES; page++) {
        const result = requestMessages(cursor, SYNC_LIMIT, syncFetchDuration);
        if (!result.ok || result.ids.length === 0) return;

        for (const id of result.ids) receivedIds.add(id);
        if (result.maxId > (session.lastId || '')) session.lastId = result.maxId;
        if (!result.hasMore) return;
        cursor = result.maxId;
    }
}

function resolveUnresolved(receivedIds) {
    if (session.unresolvedIds.length === 0) return;

    for (const id of session.unresolvedIds) {
        if (receivedIds.has(id)) liveMissed.add(1);
        else finalMissing.add(1);
    }
    session.unresolvedIds = [];
}

function observeMissed(fromId, throughId, receivedIds) {
    const missed = [];
    let cursor = rewindMessageId(fromId || '');

    for (let page = 0; page < SYNC_CATCHUP_MAX_PAGES; page++) {
        const result = requestMessages(cursor, SYNC_LIMIT, null);
        if (!result.ok || result.ids.length === 0) return missed;

        for (const id of result.ids) {
            if (id > throughId) return missed;
            if (!receivedIds.has(id)) missed.push(id);
        }
        if (!result.hasMore) return missed;
        cursor = result.maxId;
    }
    return missed;
}

function verifyAccepted(fromId, acceptedIds) {
    const missing = new Set(acceptedIds);
    const deadline = Date.now() + 60000;
    let attempt = 0;
    while (missing.size > 0 && Date.now() < deadline) {
        let cursor = rewindMessageId(fromId || '');
        for (let page = 0; page < SYNC_CATCHUP_MAX_PAGES && Date.now() < deadline; page++) {
            const result = requestMessages(cursor, SYNC_LIMIT, null, deadline - Date.now());
            if (!result.ok) break;
            for (const id of result.ids) missing.delete(id);
            if (missing.size === 0 || !result.hasMore) break;
            cursor = result.maxId;
        }
        if (missing.size === 0) break;
        const wait = Math.min(Math.max(0, deadline - Date.now()), 10000, 250 * Math.pow(2, Math.min(attempt++, 6)));
        sleep(wait * (0.5 + Math.random() / 2) / 1000);
    }
    acceptedMissing.add(missing.size);
}

function requestMessages(afterId, limit, metric, timeoutMs = 5000) {
    const query = afterId ? `?after_id=${afterId}&limit=${limit}` : `?limit=${limit}`;
    try {
        const res = http.get(`${BASE_URL}${PATHS.MESSAGES(session.roomId)}${query}`, {
            headers: authHeaders(), timeout: `${Math.min(5000, Math.max(1, timeoutMs))}ms`,
        });
        if (res.status === 200) {
            if (metric) metric.add(res.timings.duration);
            const messages = res.json('messages') || [];
            const ids = messages.map((m) => m.id).filter(Boolean);
            const maxId = ids.reduce((max, id) => (id > max ? id : max), afterId || '');
            return { ok: true, ids, maxId, hasMore: Boolean(res.json('has_more')) };
        }
    } catch (_) { }
    syncErrors.add(1);
    return { ok: false, ids: [], maxId: afterId || '', hasMore: false };
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

function withForwardedFor(headers) {
    return session.fakeIp ? { ...headers, 'X-Forwarded-For': session.fakeIp } : headers;
}

function authHeaders(extra = {}) {
    return withForwardedFor({ ...extra, Authorization: `Bearer ${session.token}` });
}

function randomBetween(min, max) {
    if (max <= min) return min;
    return min + Math.random() * (max - min);
}

export function handleSummary(data) {
    return { stdout: `summary:${JSON.stringify(data)}\n` };
}
