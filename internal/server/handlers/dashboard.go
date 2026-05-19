package handlers

import (
	_ "embed"
	"net/http"
)

// dashboardHTML is the single-file admin UI, embedded at compile time so the
// binary stays self-contained (no external static-file directory needed).
//
//go:embed dashboard.html
var dashboardHTML []byte

// Dashboard serves the HTML admin panel. It's intentionally registered outside
// the auth guard: the page is just assets (HTML/CSS/JS). The page itself
// prompts the user for an API key and then calls the authenticated REST API
// (/admin/*, /emby/*) using that key as ?api_key= on each request.
type Dashboard struct{}

// NewDashboard constructs the dashboard handler.
func NewDashboard() *Dashboard { return &Dashboard{} }

// Page serves the admin UI HTML.
// GET /admin/ui
func (d *Dashboard) Page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}

// WebStub answers /web/index.html so Emby-style deep links (used by MoviePilot's
// "open in server" button) don't 404. The page parses the hash fragment and
// shows a minimal landing screen pointing at the admin UI.
func (d *Dashboard) WebStub(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(webStubHTML))
}

const webStubHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>Emotion</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{margin:0;font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#101828;color:#e6e9ef;min-height:100vh;display:flex;align-items:center;justify-content:center}
.box{max-width:520px;padding:32px;background:#1d2435;border-radius:12px;box-shadow:0 8px 24px rgba(0,0,0,.3)}
h1{margin:0 0 12px;font-size:20px}
p{margin:8px 0;line-height:1.5;color:#aab2c5}
code{background:#0c1220;padding:2px 6px;border-radius:4px;color:#7dd3fc;font-size:13px}
a{color:#7dd3fc;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="box">
<h1>Emotion 媒体服务器</h1>
<p id="target">正在解析链接…</p>
<p>本服务为 Emby 兼容 API，没有内置 Web 播放器。请使用 Emby 客户端或返回 <a href="/admin/ui">管理面板</a>。</p>
</div>
<script>
(function(){
  var hash = window.location.hash || "";
  var m = hash.match(/[?&]id=([^&]+)/);
  var el = document.getElementById("target");
  if (m && m[1]) {
    el.textContent = "请求项: " + decodeURIComponent(m[1]);
  } else {
    el.textContent = "未识别到具体项 ID。";
  }
})();
</script>
</body>
</html>`
