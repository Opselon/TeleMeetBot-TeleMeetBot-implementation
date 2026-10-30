package dashboard

import (
	"fmt"
	"strings"
)

// The UI is one embedded, dependency-free page. Server-side state decides
// nothing: the page checks /api/state and renders the login view on 401, the
// app view otherwise. "Very light" is a feature — the whole UI is this file.

const uiStyle = `
:root{--bg:#0f172a;--panel:#1e293b;--line:#334155;--txt:#f8fafc;--dim:#94a3b8;--ok:#10b981;--warn:#f59e0b;--err:#ef4444;--acc:#3b82f6}
*{box-sizing:border-box;margin:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:var(--bg);color:var(--txt);min-height:100vh;padding:1rem}
a{color:var(--acc)}
.wrap{max-width:1100px;margin:0 auto}
h1{font-size:1.25rem;margin-bottom:.75rem}
.card{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:1rem;margin-bottom:1rem}
.dim{color:var(--dim);font-size:.85rem}
.ok{color:var(--ok)}.warn{color:var(--warn)}.err{color:var(--err)}
.row{display:flex;gap:.5rem;flex-wrap:wrap;align-items:center}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:1rem}
input,select,button{font:inherit;border-radius:8px;border:1px solid var(--line);background:#0b1220;color:var(--txt);padding:.45rem .7rem}
button{cursor:pointer;background:var(--acc);border-color:var(--acc);color:#fff}
button.ghost{background:transparent;border-color:var(--line);color:var(--txt)}
button.danger{background:var(--err);border-color:var(--err)}
button:disabled{opacity:.5;cursor:default}
table{width:100%;border-collapse:collapse;font-size:.85rem}
th,td{text-align:left;padding:.35rem .5rem;border-bottom:1px solid var(--line)}
pre{background:#0b1220;border:1px solid var(--line);border-radius:8px;padding:.75rem;overflow:auto;max-height:420px;font-size:.78rem;line-height:1.45}
.badge{display:inline-block;padding:.1rem .5rem;border-radius:99px;background:var(--line);font-size:.75rem}
.bar{height:8px;background:var(--line);border-radius:4px;overflow:hidden}
.bar>span{display:block;height:100%;background:var(--acc)}
.pill{padding:.35rem .6rem;border-radius:8px;border:1px solid var(--line);font-size:.85rem}
`

const uiScript = `
const $=(q)=>document.querySelector(q);
let filter={level:-1,component:""};
async function api(path,opts){const r=await fetch(path,Object.assign({headers:{"Content-Type":"application/json"}},opts||{}));
 if(r.status===401){showLogin();throw new Error("unauthorized")}
 if(!r.ok){const b=await r.json().catch(()=>({error:r.statusText}));throw new Error(b.error||("HTTP "+r.status))}
 return r.json()}
function showLogin(){$("#login").style.display="block";$("#app").style.display="none"}
function showApp(){$("#login").style.display="none";$("#app").style.display="block";refresh();setInterval(refresh,3000)}
async function refresh(){
 try{
  const st=await api("/api/state");render(st);
 }catch(e){if(e.message!=="unauthorized")console.warn(e)}
}
function fmtDur(s){if(!s)return"—";const m=Math.floor(s/60);const ss=Math.floor(s%60);return m+":"+(ss<10?"0":"")+ss}
function esc(t){const d=document.createElement("div");d.textContent=t==null?"":String(t);return d.innerHTML}
function stageLine(sts){if(!sts||!sts.length)return"";return sts.map(x=>esc(x.name)+" "+(x.seconds<0?"?":(Math.round(x.seconds*10)/10)+"s")+(x.active?"*":"")).join(" · ")}
function stateBadge(s){const cls=s==="streaming"?"ok":(s==="error"||s==="failed"?"err":(s==="starting"||s==="joining"?"warn":"dim"));return '<span class="'+cls+'">'+esc(s)+"</span>"}
function render(st){
 if(st.log_stats){const ls=st.log_stats;$("#logstats").textContent=ls.captured+"/"+ls.capacity+" entries · "+ls.errors+" errors · "+ls.warnings+" warnings"}
 const h=st.health||{};$("#health").innerHTML=
  '<span class="pill">v'+esc(h.version||"?")+'</span> <span class="pill">uptime '+Math.round((h.uptime_seconds||0)/60)+"m</span>"+
  ' <span class="pill">sessions '+(st.active_sessions||0)+"/"+esc(h.max_sessions||0)+"</span>"+
  ' <span class="pill">disk '+esc(h.disk_free_gb||0)+" GiB free</span>";
 const secs=st.sessions||[];
 $("#sessions").innerHTML=secs.length?"":"<p class='dim'>No sessions. Start one below.</p>";
 for(const s of secs){
  const el=document.createElement("div");el.className="card";
  let dl="";
  if(s.download&&s.download.active){const p=s.download.percent>=0?s.download.percent:0;
   dl='<div class="bar" style="margin:.4rem 0"><span style="width:'+p+'%"></span></div><div class="dim">'+esc(s.download.stage||"download")+" "+p+"%</div>"}
  el.innerHTML="<div class='row'><b>"+esc(s.id)+"</b>"+stateBadge(s.state)+
   "<span class='dim'>"+esc(s.detail||"")+"</span>"+
   "<span style='margin-left:auto'></span>"+
   (s.has_prev?'<button data-a="previous" data-id="'+s.id+'">⏮</button>':"")+
   (s.paused?'<button data-a="resume" data-id="'+s.id+'">▶</button>':'<button data-a="pause" data-id="'+s.id+'">⏸</button>')+
   (s.has_next?'<button data-a="next" data-id="'+s.id+'">⏭</button>':"")+
   '<button data-a="restart" data-id="'+s.id+'" class="ghost">🔁</button>'+
   '<button data-a="stop" data-id="'+s.id+'" class="danger">⏹</button></div>'+
   (s.title?"<div class='dim'>🎬 "+esc(s.title)+" "+fmtDur(s.cur)+" / "+fmtDur(s.dur)+"</div>":"")+
   "<div class='dim'>🔊 "+s.volume+"% · ⚡ "+(s.rate||1)+"x"+(s.muted?" · 🔇":"")+(s.play_mode?" · "+esc(s.play_mode):"")+"</div>"+dl+
   (s.stages&&s.stages.length?"<div class='dim'>🧭 "+stageLine(s.stages)+"</div>":"")+
   (s.error?"<div class='err'>🔴 "+esc(s.error)+"</div>":"");
  el.querySelectorAll("button").forEach(b=>b.onclick=()=>doAction(b.dataset.id,b.dataset.a));
  $("#sessions").appendChild(el);
 }
 const us=st.users||[];
 $("#users").innerHTML=us.length?us.map(u=>"<tr><td>"+esc(u.display_name||u.username||u.user_id)+"</td><td>"+esc(u.state)+"</td><td class='dim'>"+esc(u.detail||"")+"</td></tr>").join(""):"<tr><td class='dim' colspan=3>nobody active</td></tr>";
}
async function doAction(id,a){
 if(a==="stop"&&!confirm("Stop session "+id+"?"))return;
 try{
  let body={action:a};
  if(a==="volume"){const v=prompt("Volume 0-100","50");if(v==null)return;body={action:"volume",arg:Number(v)}}
  await api("/api/session/action?id="+encodeURIComponent(id),{method:"POST",body:JSON.stringify(body)});
  refresh();
 }catch(e){alert(e.message)}
}
async function startStream(){
 const meet=$("#meet").value.trim(),media=$("#media").value.trim(),mode=$("#mode").value;
 if(!meet||!media){alert("Both a Meet link and a media link are required");return}
 try{await api("/api/session/start",{method:"POST",body:JSON.stringify({meet_url:meet,media_url:media,mode:mode})});
  $("#meet").value="";$("#media").value="";refresh();
 }catch(e){alert(e.message)}
}
async function logout(){await api("/api/auth/logout",{method:"POST"});location.reload()}
// log stream (SSE)
function startLogStream(){
 const src=new EventSource("/api/logs/stream");
 src.onmessage=(ev)=>{try{appendLog(JSON.parse(ev.data))}catch(_){}};
 src.onerror=()=>{src.close();setTimeout(startLogStream,3000)};
}
function appendLog(e){
 const el=$("#logs");
 const lvl=e.level||"info";
 const line=document.createElement("div");
 line.innerHTML='<span class="dim">'+new Date(e.time).toLocaleTimeString()+'</span> <span class="'+(lvl==="error"?"err":(lvl==="warn"?"warn":"dim"))+'">'+esc(lvl)+"</span> "+(e.component?'<span class="badge">'+esc(e.component)+"</span> ":"")+esc(e.message);
 el.appendChild(line);
 while(el.childElementCount>400)el.removeChild(el.firstChild);
 el.scrollTop=el.scrollHeight;
}
window.addEventListener("DOMContentLoaded",()=>{
 fetch("/api/state").then(r=>{if(r.ok)showApp();else showLogin()}).catch(showLogin);
 $("#startBtn").onclick=startStream;
 $("#logoutBtn").onclick=logout;
 $("#logClear").onclick=()=>{$("#logs").innerHTML=""};
 startLogStream();
});
`

// indexHTML renders the single-page app. botUsername is embedded for the
// Telegram Login Widget (empty = only the manual payload endpoint renders).
func indexHTML(botUsername, publicBaseURL string) string {
	widget := ""
	if strings.TrimSpace(botUsername) != "" {
		widget = fmt.Sprintf(`
<script src="https://telegram.org/js/telegram-widget.js?22" data-telegram-login="%s" data-size="large" data-userpic="false" data-radius="8" data-request-access="write" data-onauth="onTelegramAuth(user)"></script>`, botUsername)
	}
	baseHint := ""
	if strings.TrimSpace(publicBaseURL) != "" {
		baseHint = "<p class='dim'>Dashboard origin: " + strings.TrimSpace(publicBaseURL) + "</p>"
	}
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TeleMeetBot Dashboard</title>
<style>` + uiStyle + `</style>
</head>
<body>
<div class="wrap">
<section id="login" style="display:none">
  <div class="card" style="max-width:480px;margin:15vh auto;text-align:center">
    <h1>🤖 TeleMeetBot</h1>
    <p class="dim" style="margin:.5rem 0 1.25rem">Sign in with your Telegram admin account.</p>
` + widget + `
    <p class="dim" style="margin-top:1rem">The widget opens Telegram's own authorization page; this server verifies the signed payload against the bot token and the admin allow-list.</p>
    <p class="dim" style="margin-top:.75rem">The bot's @` + botUsername + ` domain must be registered via @BotFather → /setdomain for the widget to render.</p>
` + baseHint + `
  </div>
</section>
<section id="app" style="display:none">
  <div class="row" style="margin-bottom:1rem">
    <h1 style="margin:0">🤖 TeleMeetBot</h1>
    <span style="margin-left:auto"></span>
    <span id="logstats" class="dim"></span>
    <button class="ghost" id="logoutBtn">Log out</button>
  </div>
  <div class="card"><div id="health" class="row"></div></div>
  <div class="card">
    <b>Start a stream</b>
    <div class="row" style="margin-top:.5rem">
      <input id="meet" placeholder="https://meet.google.com/abc-defg-hij" style="flex:1;min-width:240px">
      <input id="media" placeholder="https://youtube.com/watch?v=… or direct media URL" style="flex:1;min-width:240px">
      <select id="mode"><option value="">Auto mode</option><option value="browser">Browser</option><option value="download">Download &amp; Stream</option><option value="auto">Auto</option></select>
      <button id="startBtn">▶ Start</button>
    </div>
  </div>
  <div id="sessions" class="grid"></div>
  <div class="grid">
    <div class="card">
      <b>Users</b>
      <table style="margin-top:.5rem"><thead><tr><th>User</th><th>State</th><th>Detail</th></tr></thead><tbody id="users"></tbody></table>
    </div>
    <div class="card">
      <div class="row"><b>Live logs</b><span style="margin-left:auto"></span><button class="ghost" id="logClear">Clear view</button></div>
      <pre id="logs" style="margin-top:.5rem"></pre>
    </div>
  </div>
</section>
</div>
<script>` + uiScript + `</script>
</body>
</html>`
}
