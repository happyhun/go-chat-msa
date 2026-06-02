import http from 'k6/http';
import ws from 'k6/ws';
import { sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const API_HOST = __ENV.API_HOST || 'api-gateway';
const WS_HOST = __ENV.WS_HOST || 'ws-gateway';
const API_PORT = __ENV.API_PORT || '8080';
const WS_PORT = __ENV.WS_PORT || '8088';
const BASE_URL = `http://${API_HOST}:${API_PORT}`;
const WS_URL = `ws://${WS_HOST}:${WS_PORT}/ws`;

const PATHS = {
    HEALTH: '/health',
    ROOMS: '/rooms',
    SIGNUP: '/users',
    LOGIN: '/auth/token',
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
const SYNC_GAP_RETRIES = 3;
const SYNC_GAP_RETRY_MIN_MS = 500;
const SYNC_GAP_RETRY_MAX_MS = 3500;

const msgLatency = new Trend('msg_latency', true);
const historyFetchDuration = new Trend('history_fetch_duration', true);
const syncFetchDuration = new Trend('sync_fetch_duration', true);

const authErrors = new Counter('auth_errors');
const joinErrors = new Counter('join_errors');
const ticketErrors = new Counter('ticket_errors');
const wsConnectErrors = new Counter('ws_connect_errors');
const wsConnectRetries = new Counter('ws_connect_retries');
const msgTimeouts = new Counter('msg_timeouts');
const wsSequenceGaps = new Counter('ws_sequence_gaps');
const wsSequenceDuplicates = new Counter('ws_sequence_duplicates');
const wsSequenceRegressions = new Counter('ws_sequence_regressions');
const syncSequenceGaps = new Counter('sync_sequence_gaps');
const syncSequenceDuplicates = new Counter('sync_sequence_duplicates');
const syncSequenceRegressions = new Counter('sync_sequence_regressions');
const syncGapObserved = new Counter('sync_gap_observed');
const syncGapRecovered = new Counter('sync_gap_recovered');
const syncGapDiscarded = new Counter('sync_gap_discarded');
const syncGapRetryAttempts = new Counter('sync_gap_retry_attempts');

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
        ws_sequence_duplicates: ['count<1'],
        ws_sequence_regressions: ['count<1'],
        sync_sequence_duplicates: ['count<1'],
        sync_sequence_regressions: ['count<1'],
        sync_gap_discarded: ['count<1'],
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
    lastSeq: null,
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

    fetchHistory();

    chatOverWebSocket();
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
        return true;
    } catch (_) {
        authErrors.add(1);
        return false;
    }
}

function acquireTicket() {
    try {
        const ticketRes = retryWithBackoff(() => {
            const res = http.post(`http://${WS_HOST}:${WS_PORT}/ws/ticket`, null, {
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

function chatOverWebSocket() {
    for (let attempt = 0; attempt <= WS_CONNECT_RETRIES; attempt++) {
        const ticket = acquireTicket();
        if (!ticket) {
            sleep(5);
            return;
        }

        const status = connectWebSocket(ticket);
        if (status === 101) return;
        if (status === 503 && attempt < WS_CONNECT_RETRIES) {
            wsConnectRetries.add(1);
            sleep(randomBetween(WS_CONNECT_RETRY_MIN_MS, WS_CONNECT_RETRY_MAX_MS) / 1000);
            continue;
        }

        wsConnectErrors.add(1);
        return;
    }
}

function connectWebSocket(ticket) {
    const connUrl = `${WS_URL}?ticket=${ticket}&room_id=${session.roomId}`;
    const pending = new Map();
    let liveSeq = null;

    const connRes = ws.connect(connUrl, { headers: withForwardedFor({}) }, function (socket) {
        socket.on('open', function () {
            socket.setTimeout(function () { socket.close(); }, WS_SESSION_DURATION);
            socket.setInterval(function () {
                session.msgCount++;
                const clientMsgId = `${RUN_ID}-${__VU}-${session.msgCount}`;
                socket.send(JSON.stringify({ type: 'chat', content: MSG_BODY, client_msg_id: clientMsgId }));
                pending.set(clientMsgId, Date.now());
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
                liveSeq = observeLiveSequence(msg, liveSeq);
                if (msg.client_msg_id) {
                    const sentAt = pending.get(msg.client_msg_id);
                    if (sentAt) {
                        msgLatency.add(Date.now() - sentAt);
                        pending.delete(msg.client_msg_id);
                    }
                }
            } catch (_) { }
        });
    });

    return connRes.status;
}

function observeLiveSequence(msg, liveSeq) {
    const seq = Number(msg.sequence_number || 0);
    if (!seq) return liveSeq;

    if (liveSeq !== null) {
        if (seq === liveSeq) {
            wsSequenceDuplicates.add(1);
        } else if (seq < liveSeq) {
            wsSequenceRegressions.add(1);
        } else if (seq > liveSeq + 1) {
            wsSequenceGaps.add(seq - liveSeq - 1);
            session.lastSeq = Math.max(session.lastSeq || 0, syncWithGapRetries(liveSeq, liveSeq + 1));
        }
    }

    session.lastSeq = Math.max(session.lastSeq || 0, seq);
    return Math.max(liveSeq || 0, seq);
}

function fetchHistory() {
    if (session.lastSeq !== null) {
        session.lastSeq = Math.max(session.lastSeq, syncWithGapRetries(session.lastSeq, null));
        return;
    }

    const result = requestMessages(null, 50, historyFetchDuration);
    if (result.ok && result.messages.length > 0) session.lastSeq = result.maxSeq;
}

function syncWithGapRetries(lastSeq, requiredSeq) {
    const attempts = SYNC_GAP_RETRIES + 1;
    let maxSeq = lastSeq;
    let observedGap = false;

    for (let attempt = 0; attempt < attempts; attempt++) {
        const result = requestMessages(lastSeq, SYNC_LIMIT, syncFetchDuration);
        if (!result.ok) break;

        maxSeq = Math.max(maxSeq, result.maxSeq);
        const validation = validateSyncedMessages(result.messages, lastSeq);
        if (validation.gapCount > 0) {
            syncGapObserved.add(validation.gapCount);
            observedGap = true;
        }

        const recoveredRequired = requiredSeq === null ||
            result.messages.some((m) => Number(m.sequence_number || 0) === requiredSeq);
        if (validation.gapCount === 0 && recoveredRequired) {
            if (observedGap) syncGapRecovered.add(1);
            return maxSeq;
        }

        if (requiredSeq !== null && !recoveredRequired) observedGap = true;
        if (attempt + 1 >= attempts) break;

        syncGapRetryAttempts.add(1);
        sleep(randomBetween(SYNC_GAP_RETRY_MIN_MS, SYNC_GAP_RETRY_MAX_MS) / 1000);
    }

    if (observedGap) syncGapDiscarded.add(1);
    return maxSeq;
}

function requestMessages(lastSeq, limit, metric) {
    const query = lastSeq === null ? `?limit=${limit}` : `?last_seq=${lastSeq}&limit=${limit}`;
    try {
        const res = http.get(`${BASE_URL}${PATHS.MESSAGES(session.roomId)}${query}`, {
            headers: authHeaders(),
        });
        if (res.status === 200) {
            metric.add(res.timings.duration);
            const messages = res.json('messages') || [];
            const maxSeq = messages.length > 0
                ? messages.reduce((max, m) => Math.max(max, Number(m.sequence_number || 0)), lastSeq || 0)
                : lastSeq || 0;
            return { ok: true, messages, maxSeq };
        }
    } catch (_) { }
    return { ok: false, messages: [], maxSeq: lastSeq || 0 };
}

function validateSyncedMessages(messages, lastSeq) {
    if (!messages || messages.length === 0 || lastSeq === null) return { gapCount: 0 };
    if (lastSeq <= 0) return { gapCount: 0 };

    const sequences = messages
        .map((msg) => Number(msg.sequence_number || 0))
        .filter((seq) => seq > 0)
        .sort((a, b) => a - b);

    let expected = lastSeq + 1;
    let gapCount = 0;
    let previous = null;
    for (const seq of sequences) {
        if (previous !== null && seq === previous) {
            syncSequenceDuplicates.add(1);
            continue;
        }
        if (seq < expected) {
            syncSequenceRegressions.add(1);
        } else if (seq > expected) {
            gapCount += seq - expected;
        }
        expected = Math.max(expected, seq + 1);
        previous = seq;
    }
    if (gapCount > 0) syncSequenceGaps.add(gapCount);
    return { gapCount };
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
