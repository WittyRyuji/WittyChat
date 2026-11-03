# UI smoke for the hardening batch — drives WebView2 over CDP.
# -*- coding: utf-8 -*-
import json, sys, time, urllib.request
import websocket

sys.stdout.reconfigure(encoding="utf-8")
PORT = 9333
targets = json.load(urllib.request.urlopen(f"http://localhost:{PORT}/json"))
page = next(t for t in targets if t["type"] == "page")
ws = websocket.create_connection(page["webSocketDebuggerUrl"], suppress_origin=True, timeout=20)
_id = 0

def ev(expr, wait=0.0):
    global _id
    _id += 1
    ws.send(json.dumps({"id": _id, "method": "Runtime.evaluate",
                        "params": {"expression": expr, "returnByValue": True, "awaitPromise": True}}))
    if wait: time.sleep(wait)
    while True:
        m = json.loads(ws.recv())
        if m.get("id") == _id:
            return m.get("result", {}).get("result", {}).get("value")

def key(text):
    ev(f"""(()=>{{const el=document.activeElement;
      for(const ch of {json.dumps(text)}){{
        el.dispatchEvent(new KeyboardEvent('keydown',{{key:ch,bubbles:true}}));
        el.dispatchEvent(new KeyboardEvent('keypress',{{key:ch,bubbles:true}}));
      }}
      el.dispatchEvent(new KeyboardEvent('keydown',{{key:'Enter',bubbles:true}}));
      el.dispatchEvent(new KeyboardEvent('keyup',{{key:'Enter',bubbles:true}}));
      return document.activeElement.tagName;}})())""")

# 1. ensure logged in + a conversation open
print("logged in:", ev("!!document.querySelector('.rail')"))
if not ev("!!document.querySelector('.rail')"):
    print(ev("""(async()=>{const set=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value').set;
      const [e,p]=document.querySelectorAll('input');const b=[...document.querySelectorAll('button')].find(x=>/登|login/i.test(x.textContent));
      set.call(e,'alice@wittyloong.com');e.dispatchEvent(new Event('input',{bubbles:true}));
      set.call(p,'AlicePass123!');p.dispatchEvent(new Event('input',{bubbles:true}));b.click();return 'login-sent'})()""", 1.5))
    time.sleep(2)

ev("""(()=>{const c=[...document.querySelectorAll('.conv')].find(x=>/工程部|Ryuji/.test(x.textContent))||document.querySelector('.conv');c?.click();return c?.textContent.trim().slice(0,30)})()""")
time.sleep(1.5)

# 2. header doc button: bold text, no icon
print("docs-btn:", ev("""(()=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='文档');
  if(!b) return 'MISSING';
  const cs=getComputedStyle(b);
  return {text:b.textContent.trim(), weight:cs.fontWeight, html:b.innerHTML.slice(0,80)}})()"""))

# 3. plus menu: floating, exactly two items
ev("""(()=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='＋'||x.textContent.trim()==='+');b?.click();return 1})()""")
time.sleep(0.4)
print("plus-menu:", ev("""(()=>{const m=document.querySelector('.plus-menu');
  if(!m) return 'MISSING';
  const cs=getComputedStyle(m);
  return {pos:cs.position, items:[...m.children].map(c=>c.textContent.trim())}})()"""))
ev("document.body.click()")

# 4. docs drawer: open + resize
ev("""(()=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='文档');b?.click();return 1})()""")
time.sleep(0.6)
print("drawer:", ev("""(()=>{const p=document.querySelector('.docs-panel');const h=document.querySelector('.dp-resize');
  return p?{w:p.offsetWidth, handle:!!h}:'MISSING'})()"""))
ev("""(()=>{const p=document.querySelector('.docs-panel');const h=document.querySelector('.dp-resize');if(!h)return 'noh';
  const r=h.getBoundingClientRect();
  h.dispatchEvent(new MouseEvent('mousedown',{clientX:r.x+2,clientY:r.y+2,bubbles:true}));
  document.dispatchEvent(new MouseEvent('mousemove',{clientX:r.x-80,clientY:r.y+2,bubbles:true}));
  document.dispatchEvent(new MouseEvent('mouseup',{bubbles:true}));
  return p.offsetWidth})()""")
print("drawer after drag:", ev("document.querySelector('.docs-panel')?.offsetWidth"))

# 5. sheet typing — create a sheet via docs panel
print("new-doc menu:", ev("""(()=>{const b=[...document.querySelectorAll('.docs-panel button')].find(x=>/新建|\\+/.test(x.textContent));b?.click();
  return [...document.querySelectorAll('.docs-panel button, .docs-panel [role=menuitem], .np-item')].map(x=>x.textContent.trim()).slice(0,8)})()"""))
time.sleep(0.4)
ev("""(()=>{const b=[...document.querySelectorAll('button,[role=menuitem],.np-item')].find(x=>/表格/.test(x.textContent));b?.click();return b?.textContent})()""")
time.sleep(1.5)
print("sheet open:", ev("!!document.querySelector('.sheet-grid, .sheet-table, table.sheet')"))
# click a cell to select, then type on the grid (first char seeds the editor)
ev("""(()=>{const c=document.querySelector('td.sheet-cell');
  c?.dispatchEvent(new MouseEvent('mousedown',{bubbles:true}));c?.click();return c?.className||'NOCELL'})()""")
time.sleep(0.3)
ev("""(()=>{const w=document.querySelector('.sheet-wrap');
  w.dispatchEvent(new KeyboardEvent('keydown',{key:'1',bubbles:true}));return 1})()""")
time.sleep(0.3)
print("editing started:", ev("!!document.querySelector('.cell-input')"))
# continue typing into the mounted cell input, then Enter commits
ev("""(()=>{const inp=document.querySelector('.cell-input');if(!inp)return 'NOINP';
  const set=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value').set;
  set.call(inp,'12');inp.dispatchEvent(new Event('input',{bubbles:true}));
  inp.dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',bubbles:true}));return 'ok'})()""")
time.sleep(0.5)
print("cell after commit:", ev("""(()=>{const cells=[...document.querySelectorAll('td.sheet-cell')];
  const fx=document.querySelector('.sheet-fx input');
  return {a1:cells[0]?.textContent, fx:fx?.value, filled:cells.findIndex(c=>c.textContent==='12')}})()"""))

# 6. slide typing — open docs page from rail, create slide doc
ev("""(()=>{const r=[...document.querySelectorAll('.rail *')].find(x=>x.textContent.trim()==='文档');
  (r?.closest('button,div')||r)?.click();return 1})()""")
time.sleep(1.2)
print("docs page:", ev("!!document.querySelector('.docs-page')"))
ev("""(()=>{const b=[...document.querySelectorAll('.docs-page button')].find(x=>/新建|^\\+|＋/.test(x.textContent));b?.click();return b?.textContent})()""")
time.sleep(0.4)
ev("""(()=>{const b=[...document.querySelectorAll('button,[role=menuitem],li,div')].find(x=>x.textContent.trim()==='🖼 演示'||x.textContent.trim()==='演示');b?.click();return b?.textContent})()""")
time.sleep(1.5)
print("slide open:", ev("!!document.querySelector('.slide-canvas')"))
# type into slide body via native setter (textarea is controlled)
ev("""(()=>{const t=document.querySelector('.slide-body');if(!t)return 'NOBODY';
  t.focus();
  const set=Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype,'value').set;
  for(const ch of 'hello'){set.call(t,t.value+ch);t.dispatchEvent(new Event('input',{bubbles:true}));}
  return t.value})()""")
time.sleep(0.5)
print("slide body after typing:", ev("document.querySelector('.slide-body')?.value"))
# thumbnail count + switch slide nav sanity
print("slide thumbs:", ev("document.querySelectorAll('.slide-thumb').length"))
ws.close()
