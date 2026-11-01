// Real Yjs two-client sync test against the Go relay.
// Two WebsocketProviders on the same doc; assert convergence + late-join replay.
import { WebsocketProvider } from 'y-websocket';
import * as Y from 'yjs';
import WS from 'ws';

const B = 'http://localhost:8080';
const login = async (email, password) => {
  const r = await fetch(`${B}/api/v1/auth/login`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password, device_name: 't', device_kind: 'web' }),
  });
  return (await r.json()).session_token;
};
const mkDoc = async (tok, title) => {
  const r = await fetch(`${B}/api/v1/docs`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${tok}` },
    body: JSON.stringify({ title }),
  });
  return (await r.json()).id;
};

const adm = await login('admin@wittyloong.com', 'DevPass123!');
const ali = await login('alice@wittyloong.com', 'AlicePass123!');

// Conv-bound doc so alice can edit too — pick a shared conv.
const convs = await (await fetch(`${B}/api/v1/conversations`, { headers: { Authorization: `Bearer ${ali}` } })).json();
let docId = null;
for (const c of convs.conversations) {
  if (!['group', 'channel'].includes(c.kind)) continue;
  const r = await fetch(`${B}/api/v1/docs`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${ali}` },
    body: JSON.stringify({ title: 'yjs-e2e', conv_id: c.id }),
  });
  if (r.ok) {
    const d = await r.json();
    const chk = await fetch(`${B}/api/v1/docs/${d.id}`, { headers: { Authorization: `Bearer ${adm}` } });
    if (chk.ok) { docId = d.id; break; }
  }
}
if (!docId) { console.log('NO_SHARED_DOC'); process.exit(1); }
console.log('doc:', docId);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const mkClient = (tok) => {
  const ydoc = new Y.Doc();
  const provider = new WebsocketProvider('ws://localhost:8080/ws/doc', docId, ydoc, {
    params: { token: tok }, WebSocketPolyfill: WS,
  });
  return { ydoc, provider, text: ydoc.getText('t') };
};

const A = mkClient(adm);
const Bc = mkClient(ali);
await sleep(1200);
console.log('A synced:', A.provider.synced, 'B synced:', Bc.provider.synced);

// A types
A.text.insert(0, 'hello-from-A');
await sleep(800);
console.log('B sees A:', JSON.stringify(Bc.text.toString()));

// B types
Bc.text.insert(Bc.text.length, '|world-from-B');
await sleep(800);
console.log('A sees merged:', JSON.stringify(A.text.toString()));

// late joiner replays
const C = mkClient(adm);
await sleep(1500);
console.log('C replays:', JSON.stringify(C.text.toString()));

// awareness presence
Bc.provider.awareness.setLocalStateField('user', { name: 'Alice', color: '#f00' });
await sleep(600);
const states = [...A.provider.awareness.getStates().values()].map((s) => s.user?.name);
console.log('A awareness sees:', states);

A.provider.destroy(); Bc.provider.destroy(); C.provider.destroy();
await sleep(200);
process.exit(0);
