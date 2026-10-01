// Chattie web client.
//
// Postgres history is the truth; the WebSocket is only a fast path. Every
// room keeps `last`, the highest sequence up to which nothing is missing.
// Whenever the server hints that something was missed (reconnect, resync,
// heartbeat, or a gap in sequences) the client fetches everything after `last`.

const $ = (id) => document.getElementById(id);

let me = null;       // signed-in user, or null
let ws = null;
let rooms = [];      // from GET /api/rooms
let active = null;   // id of the room on screen
let shown = null;    // id of the room the message list was last drawn for
let attempts = 0;    // failed connection attempts, for backoff
let watchdog = null; // closes a socket that went silent
let lastTyping = 0;

const chats = new Map();   // room id -> { msgs: Map(id -> message), seqs: Set, first, last, loaded, busy, again, unread }
const pending = new Map(); // client_message_id -> { room_id, content, failed }
const typers = new Map();  // username -> timer that clears "is typing"

// ---------- HTTP ----------

async function api(method, path, body) {
  const request = () => fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  let res = await request();
  // Access tokens are short-lived. On 401, rotate the refresh token and retry once.
  if (res.status === 401 && !path.startsWith('/api/auth/')) {
    if (await refresh()) res = await request();
    if (res.status === 401) {
      showLogin();
      throw new Error('signed out');
    }
  }
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || res.statusText);
  return data;
}

// A refresh token works only once, so two tabs must not use the same one at
// the same time. The lock makes them take turns.
function refresh() {
  const run = () => fetch('/api/auth/refresh', { method: 'POST' }).then((res) => res.ok);
  return navigator.locks ? navigator.locks.request('chattie-refresh', run) : run();
}

// ---------- sign in ----------

function showLogin() {
  me = null;
  ws?.close();
  $('app').hidden = true;
  $('login').hidden = false;
}

$('login').onsubmit = async (e) => {
  e.preventDefault();
  const action = e.submitter?.value || 'login';
  try {
    me = await api('POST', `/api/auth/${action}`, { username: $('username').value, password: $('password').value });
    start();
  } catch (err) {
    $('login-error').textContent = err.message;
  }
};

$('logout').onclick = async () => {
  await api('POST', '/api/auth/logout').catch(() => {});
  location.reload();
};

function start() {
  $('login').hidden = true;
  $('app').hidden = false;
  $('me').textContent = me.username;
  connect();
}

// ---------- WebSocket ----------

async function connect() {
  try {
    await api('GET', '/api/me'); // renews the access cookie if it expired
  } catch {
    if (me) reconnectLater();    // network trouble, not a sign-out
    return;
  }
  ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`);
  ws.onmessage = (e) => onFrame(JSON.parse(e.data));
  ws.onclose = reconnectLater;
}

// Exponential backoff with jitter, so clients do not all return at once.
function reconnectLater() {
  clearTimeout(watchdog);
  if (!me) return;
  $('status').textContent = 'reconnecting…';
  const delay = Math.min(15000, 300 * 2 ** attempts++) * (0.5 + Math.random());
  setTimeout(connect, delay);
}

function onFrame(f) {
  // The server sends a heartbeat every 20s. Silence means a dead socket.
  clearTimeout(watchdog);
  watchdog = setTimeout(() => ws.close(), 45000);

  switch (f.type) {
    case 'hello':
      attempts = 0;
      $('status').textContent = `connected to ${f.instance}`;
      showOnline(f.online);
      sync();
      break;
    case 'message':
    case 'ack': // an ack carries the committed copy of our own message
      merge(f.message, true);
      break;
    case 'error': {
      const p = pending.get(f.client_message_id);
      if (!p) break;
      if (f.retry) setTimeout(() => transmit(f.client_message_id), 2000);
      else p.failed = f.error;
      showMessages();
      break;
    }
    case 'typing':
      if (f.room_id === active && f.username !== me.username) showTyping(f.username);
      break;
    case 'heartbeat':
      showOnline(f.online);
      for (const [id, seq] of Object.entries(f.sequences || {})) {
        if (chat(+id).last < seq) catchUp(+id);
      }
      break;
    case 'resync':
    case 'rooms_changed':
      sync();
      break;
    case 'reconnect':
      ws.close();
      break;
  }
}

// ---------- rooms and history ----------

function chat(id) {
  // first: the oldest sequence loaded. last: the newest with nothing missing before it.
  if (!chats.has(id)) chats.set(id, { msgs: new Map(), seqs: new Set(), first: Infinity, last: 0, loaded: false, busy: false, again: false, unread: false });
  return chats.get(id);
}

// sync reloads the room list, catches up every joined room and resends
// anything that was never acknowledged.
async function sync() {
  try {
    rooms = await api('GET', '/api/rooms');
    const joined = rooms.filter((r) => r.joined);
    if (!joined.some((r) => r.id === active)) active = joined[0]?.id ?? null;
    showRooms();
    showMessages();
    await Promise.all(joined.map((r) => catchUp(r.id)));
    for (const id of pending.keys()) transmit(id);
  } catch (err) {
    console.warn('sync failed', err); // the next heartbeat or reconnect retries
  }
}

// catchUp fetches what the room is missing: the newest page on first load,
// then everything after `last`, page by page.
async function catchUp(id) {
  const c = chat(id);
  if (c.busy) {
    c.again = true; // something new arrived meanwhile: run once more afterwards
    return;
  }
  c.busy = true;
  try {
    do {
      c.again = false;
      if (!c.loaded) {
        const page = await api('GET', `/api/rooms/${id}/messages?limit=50`);
        if (page.length) c.last = Math.max(c.last, page[0].sequence - 1);
        page.forEach((m) => merge(m, false));
        c.loaded = true;
      }
      let page;
      do {
        page = await api('GET', `/api/rooms/${id}/messages?after=${c.last}&limit=200`);
        page.forEach((m) => merge(m, false));
      } while (page.length === 200);
    } while (c.again);
  } finally {
    c.busy = false;
  }
  if (id === active) showMessages();
}

// loadOlder fetches the page of messages before the oldest one loaded.
async function loadOlder() {
  const id = active;
  const page = await api('GET', `/api/rooms/${id}/messages?before=${chat(id).first}&limit=50`);
  page.forEach((m) => merge(m, false));
  if (id !== active) return;
  const box = $('messages');
  const height = box.scrollHeight;
  showMessages();
  box.scrollTop += box.scrollHeight - height; // keep the same messages in view
}

// merge adds a message once (delivery may repeat) and advances `last` while
// the sequences are contiguous. A live message beyond `last + 1` reveals a gap.
// Live messages are drawn at once; callers that merge a whole page draw after.
function merge(m, live) {
  const c = chat(m.room_id);
  if (m.sender_id === me.id) pending.delete(m.client_message_id);
  if (!c.msgs.has(m.id)) {
    c.msgs.set(m.id, m);
    c.seqs.add(m.sequence);
    c.first = Math.min(c.first, m.sequence);
    while (c.seqs.has(c.last + 1)) c.last++;
    if (m.room_id !== active && c.loaded && !c.unread) {
      c.unread = true;
      showRooms();
    }
  }
  if (live && m.sequence > c.last) catchUp(m.room_id).catch(console.warn);
  if (live && m.room_id === active) showMessages();
}

// ---------- sending ----------

// A message keeps one client_message_id for its whole life. Retries reuse it,
// so the server stores the message once however many times it is sent.
function send(roomId, content) {
  const id = crypto.randomUUID();
  pending.set(id, { room_id: roomId, content });
  transmit(id);
  showMessages();
}

function transmit(id) {
  const p = pending.get(id);
  if (p && !p.failed && ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'message', room_id: p.room_id, client_message_id: id, content: p.content }));
  }
}

$('composer').onsubmit = (e) => {
  e.preventDefault();
  const content = $('draft').value.trim();
  if (!content || active === null) return;
  send(active, content);
  $('draft').value = '';
};

$('draft').oninput = () => {
  if (Date.now() - lastTyping < 2000 || ws?.readyState !== WebSocket.OPEN) return;
  lastTyping = Date.now();
  ws.send(JSON.stringify({ type: 'typing', room_id: active }));
};

// ---------- room actions ----------

async function act(fn) {
  try {
    await fn();
    await sync();
  } catch (err) {
    alert(err.message);
  }
}

async function openRoom(room) {
  if (!room.joined) await api('POST', `/api/rooms/${room.id}/join`);
  active = room.id;
  chat(room.id).unread = false;
}

$('new-room').onsubmit = (e) => {
  e.preventDefault();
  act(async () => {
    const room = await api('POST', '/api/rooms', { name: $('room-name').value });
    active = room.id;
    $('room-name').value = '';
  });
};

$('leave').onclick = () => act(() => api('POST', `/api/rooms/${active}/leave`));

$('delete').onclick = () => {
  if (confirm('Delete this room for everyone?')) act(() => api('DELETE', `/api/rooms/${active}`));
};

// ---------- rendering ----------

function el(tag, text, className) {
  const node = document.createElement(tag);
  node.textContent = text; // textContent, never innerHTML: messages are untrusted
  if (className) node.className = className;
  return node;
}

const label = (room) => (room.kind === 'dm' ? '@' : '#') + room.name;

function showRooms() {
  $('rooms').replaceChildren(...rooms.map((room) => {
    const classes = [room.id === active && 'active', chat(room.id).unread && 'unread', !room.joined && 'other'];
    const li = el('li', label(room), classes.filter(Boolean).join(' '));
    li.title = room.joined ? '' : 'Click to join';
    li.onclick = () => act(() => openRoom(room));
    return li;
  }));
}

function showMessages() {
  const room = rooms.find((r) => r.id === active);
  $('title').textContent = room ? label(room) : '';
  $('leave').hidden = !room || room.kind !== 'public' || room.owner;
  $('delete').hidden = !room?.owner;

  const c = room ? chat(active) : null;
  const rows = room ? [...c.msgs.values()].sort((a, b) => a.sequence - b.sequence) : [];
  const waiting = [...pending.values()].filter((p) => p.room_id === active);

  // Sequences start at 1, so anything above that means older messages exist.
  const older = [];
  if (c?.loaded && c.first > 1 && c.first !== Infinity) {
    const li = el('li', 'Load older messages', 'older');
    li.onclick = () => loadOlder().catch((err) => alert(err.message));
    older.push(li);
  }

  // Follow new messages only if the reader is already at the bottom.
  const box = $('messages');
  const follow = shown !== active || box.scrollHeight - box.scrollTop - box.clientHeight < 60;
  shown = active;

  box.replaceChildren(
    ...older,
    ...rows.map((m) => {
      const li = el('li');
      const time = new Date(m.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
      li.append(el('time', time), el('b', m.sender), el('span', m.content));
      return li;
    }),
    ...waiting.map((p) => {
      const li = el('li', '', p.failed ? 'failed' : 'pending');
      li.append(el('b', me.username), el('span', p.failed ? `${p.content} (${p.failed})` : p.content));
      return li;
    }),
  );
  if (follow) box.scrollTop = box.scrollHeight;
}

function showOnline(names) {
  if (!names) return; // presence is unavailable: keep the last list
  $('online').replaceChildren(...names.map((name) => {
    const li = el('li', name);
    if (name !== me.username) {
      li.title = 'Click to message';
      li.onclick = () => act(async () => {
        const room = await api('POST', '/api/dms', { username: name });
        active = room.id;
      });
    }
    return li;
  }));
}

function showTyping(name) {
  clearTimeout(typers.get(name));
  typers.set(name, setTimeout(() => {
    typers.delete(name);
    $('typing').textContent = [...typers.keys()].join(', ') + (typers.size ? ' typing…' : '');
  }, 3000));
  $('typing').textContent = [...typers.keys()].join(', ') + ' typing…';
}

// ---------- startup ----------

try {
  me = await api('GET', '/api/me');
  start();
} catch {
  showLogin();
}
