# Focused re-check: conv header doc btn / plus menu / drawer, starting from docs page.
import json, sys, time, urllib.request
import websocket
sys.stdout.reconfigure(encoding="utf-8")
t = next(x for x in json.load(urllib.request.urlopen("http://localhost:9333/json")) if x["type"] == "page")
ws = websocket.create_connection(t["webSocketDebuggerUrl"], suppress_origin=True, timeout=15)
_id = 0
def ev(e):
    global _id; _id += 1
    ws.send(json.dumps({"id": _id, "method": "Runtime.evaluate", "params": {"expression": e, "returnByValue": True}}))
    while True:
        m = json.loads(ws.recv())
        if m.get("id") == _id:
            return m.get("result", {}).get("result", {}).get("value")

# back to chat view via rail (first rail item = chat)
print("nav:", ev("[...document.querySelectorAll('.rail-item')].map(x=>x.getAttribute('title')||x.textContent.trim()||x.className)"))
ev("document.querySelectorAll('.rail-item')[0]?.click()")
time.sleep(1.2)
ev("""(()=>{const c=[...document.querySelectorAll('.conv')].find(x=>/工程部|Ryuji/.test(x.textContent))||document.querySelector('.conv');c?.click();return 'conv:'+c?.textContent.trim().slice(0,20)})()""")
time.sleep(1.5)
print("docs-btn:", ev("""(()=>{const b=[...document.querySelectorAll('.conv-head button')].find(x=>x.textContent.trim()==='文档');
  if(!b) return 'MISSING'; const cs=getComputedStyle(b);
  return {text:b.textContent.trim(), weight:cs.fontWeight, html:b.innerHTML.slice(0,60)}})()"""))
ev("""(()=>{const b=[...document.querySelectorAll('.composer button, .send-bar button, [class*=composer] button')].find(x=>/^\\+?$|^＋$/.test(x.textContent.trim()));b?.click();return b?.textContent})()""")
time.sleep(0.4)
print("plus-menu:", ev("""(()=>{const m=document.querySelector('.plus-menu');
  if(!m) return 'MISSING'; const cs=getComputedStyle(m); const r=m.getBoundingClientRect();
  return {pos:cs.position, items:[...m.children].map(c=>c.textContent.trim()), above:r.y}})()"""))
ev("document.body.click()")
ev("""(()=>{const b=[...document.querySelectorAll('.conv-head button')].find(x=>x.textContent.trim()==='文档');b?.click();return 1})()""")
time.sleep(0.6)
w0 = ev("document.querySelector('.docs-panel')?.offsetWidth")
ev("""(()=>{const h=document.querySelector('.dp-resize');const r=h.getBoundingClientRect();
  h.dispatchEvent(new MouseEvent('mousedown',{clientX:r.x+2,clientY:r.y+2,bubbles:true}));
  document.dispatchEvent(new MouseEvent('mousemove',{clientX:r.x-60,clientY:r.y+2,bubbles:true}));
  document.dispatchEvent(new MouseEvent('mouseup',{bubbles:true}));return 1})()""")
print("drawer drag:", w0, "->", ev("document.querySelector('.docs-panel')?.offsetWidth"))
ws.close()
