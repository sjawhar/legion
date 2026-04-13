/**
 * Single-page dashboard UI served as inline HTML/CSS/JS.
 * Fetches GET /dashboard JSON and renders worker status, pipeline, tokens.
 * Auto-refreshes every 30 seconds. No framework dependencies.
 */
export function getDashboardHtml(): string {
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Legion Dashboard</title>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0d1117;--surface:#161b22;--border:#30363d;--text:#e6edf3;
  --text-muted:#8b949e;--accent:#58a6ff;--green:#3fb950;--yellow:#d29922;
  --red:#f85149;--orange:#db6d28;--purple:#bc8cff;
  --radius:8px;--gap:12px;
}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif;
  background:var(--bg);color:var(--text);line-height:1.5;min-height:100vh}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}

.container{max-width:1200px;margin:0 auto;padding:var(--gap)}
header{display:flex;align-items:center;justify-content:space-between;
  padding:var(--gap);border-bottom:1px solid var(--border);flex-wrap:wrap;gap:8px}
header h1{font-size:1.25rem;font-weight:600}
.meta{font-size:0.8rem;color:var(--text-muted);display:flex;align-items:center;gap:8px}
.refresh-indicator{width:8px;height:8px;border-radius:50%;background:var(--green);
  display:inline-block;transition:background 0.3s}
.refresh-indicator.fetching{background:var(--yellow);animation:pulse 1s infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:0.4}}

/* Summary cards */
.summary{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));
  gap:var(--gap);margin:var(--gap) 0}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);
  padding:var(--gap)}
.card-label{font-size:0.75rem;color:var(--text-muted);text-transform:uppercase;
  letter-spacing:0.05em;margin-bottom:4px}
.card-value{font-size:1.5rem;font-weight:700}
.card-detail{font-size:0.75rem;color:var(--text-muted);margin-top:4px}

/* Pipeline */
.pipeline{display:flex;gap:4px;align-items:center;flex-wrap:wrap;margin:var(--gap) 0;
  padding:var(--gap);background:var(--surface);border:1px solid var(--border);
  border-radius:var(--radius);overflow-x:auto}
.pipeline-stage{padding:6px 12px;border-radius:16px;font-size:0.75rem;font-weight:600;
  white-space:nowrap;background:var(--border);color:var(--text-muted);position:relative}
.pipeline-stage.active{background:var(--accent);color:#fff}
.pipeline-stage .count{margin-left:4px;opacity:0.8}
.pipeline-arrow{color:var(--text-muted);font-size:0.7rem}

/* Repo groups */
.repo-group{margin:var(--gap) 0}
.repo-header{font-size:0.9rem;font-weight:600;color:var(--text-muted);
  padding:8px 0;border-bottom:1px solid var(--border);margin-bottom:8px}

/* Issue row */
.issue{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);
  margin-bottom:var(--gap);overflow:hidden}
.issue-header{display:flex;align-items:center;gap:8px;padding:10px var(--gap);
  border-bottom:1px solid var(--border);flex-wrap:wrap}
.issue-number{font-weight:700;color:var(--accent);font-size:0.9rem}
.issue-title{font-size:0.85rem;color:var(--text);flex:1;min-width:100px}
.issue-status{font-size:0.7rem;padding:2px 8px;border-radius:12px;font-weight:600;
  white-space:nowrap}

/* Worker row */
.worker{display:grid;grid-template-columns:1fr auto;gap:8px;padding:8px var(--gap);
  border-bottom:1px solid var(--border);align-items:center}
.worker:last-child{border-bottom:none}
.worker-info{display:flex;align-items:center;gap:8px;flex-wrap:wrap;min-width:0}
.worker-phase{font-size:0.75rem;padding:2px 8px;border-radius:12px;font-weight:600;
  white-space:nowrap}
.worker-status{font-size:0.7rem;padding:2px 6px;border-radius:10px;font-weight:600}
.worker-id{font-size:0.75rem;color:var(--text-muted);overflow:hidden;text-overflow:ellipsis;
  white-space:nowrap}
.worker-activity{display:flex;gap:12px;font-size:0.75rem;color:var(--text-muted);
  flex-wrap:wrap;justify-content:flex-end}
.worker-activity span{white-space:nowrap}

/* Status colors */
.status-running{background:rgba(63,185,80,0.15);color:var(--green)}
.status-starting{background:rgba(210,153,34,0.15);color:var(--yellow)}
.status-stopped{background:rgba(139,148,158,0.15);color:var(--text-muted)}
.status-dead{background:rgba(248,81,73,0.15);color:var(--red)}

/* Phase colors */
.phase-architect{background:rgba(188,140,255,0.15);color:var(--purple)}
.phase-plan{background:rgba(88,166,255,0.15);color:var(--accent)}
.phase-implement{background:rgba(63,185,80,0.15);color:var(--green)}
.phase-test{background:rgba(210,153,34,0.15);color:var(--yellow)}
.phase-review{background:rgba(219,109,40,0.15);color:var(--orange)}
.phase-merge{background:rgba(139,148,158,0.15);color:var(--text-muted)}

/* Issue status colors */
.issue-status-triage{background:rgba(139,148,158,0.15);color:var(--text-muted)}
.issue-status-todo{background:rgba(88,166,255,0.15);color:var(--accent)}
.issue-status-in-progress{background:rgba(63,185,80,0.15);color:var(--green)}
.issue-status-testing{background:rgba(210,153,34,0.15);color:var(--yellow)}
.issue-status-needs-review{background:rgba(219,109,40,0.15);color:var(--orange)}
.issue-status-done{background:rgba(139,148,158,0.15);color:var(--text-muted)}
.issue-status-retro{background:rgba(188,140,255,0.15);color:var(--purple)}

/* Events */
.events{margin:var(--gap) 0}
.events-title{font-size:0.85rem;font-weight:600;color:var(--text-muted);
  margin-bottom:8px;cursor:pointer;user-select:none}
.events-title::before{content:"\\25B6 ";font-size:0.65rem}
.events-title.open::before{content:"\\25BC "}
.events-list{display:none;background:var(--surface);border:1px solid var(--border);
  border-radius:var(--radius);max-height:300px;overflow-y:auto}
.events-list.open{display:block}
.event-row{display:flex;gap:8px;padding:6px var(--gap);border-bottom:1px solid var(--border);
  font-size:0.75rem;align-items:baseline;flex-wrap:wrap}
.event-row:last-child{border-bottom:none}
.event-time{color:var(--text-muted);white-space:nowrap;min-width:60px}
.event-type{font-weight:600;white-space:nowrap}
.event-detail{color:var(--text-muted);overflow:hidden;text-overflow:ellipsis}

/* Empty state */
.empty{text-align:center;padding:48px var(--gap);color:var(--text-muted)}
.empty-icon{font-size:2rem;margin-bottom:8px}
.empty-text{font-size:0.9rem}

/* Error banner */
.error-banner{background:rgba(248,81,73,0.1);border:1px solid var(--red);color:var(--red);
  padding:8px var(--gap);border-radius:var(--radius);margin:var(--gap) 0;font-size:0.85rem;
  display:none}
.error-banner.visible{display:block}

/* Countdown */
.countdown{font-variant-numeric:tabular-nums}

/* Responsive */
@media(max-width:600px){
  header{flex-direction:column;align-items:flex-start}
  .summary{grid-template-columns:repeat(2,1fr)}
  .worker{grid-template-columns:1fr}
  .worker-activity{justify-content:flex-start;margin-top:4px}
  .pipeline{justify-content:center}
}
</style>
</head>
<body>
<header>
  <h1>Legion Dashboard</h1>
  <div class="meta">
    <span class="refresh-indicator" id="indicator"></span>
    <span>Auto-refresh <span class="countdown" id="countdown">30</span>s</span>
    <span id="updated"></span>
  </div>
</header>
<div class="container">
  <div class="error-banner" id="error"></div>
  <div class="summary" id="summary"></div>
  <div id="pipeline"></div>
  <div id="groups"></div>
  <div class="events" id="events-section"></div>
</div>
<script>
(function(){
  "use strict";
  var REFRESH_INTERVAL = 30;
  var countdown = REFRESH_INTERVAL;
  var timer = null;
  var eventsOpen = false;

  var PHASES = ["architect","plan","implement","test","review","merge"];

  function $(id){ return document.getElementById(id); }
  function esc(s){ var d = document.createElement("div"); d.textContent = s; return d.innerHTML; }
  function relTime(iso){
    if(!iso) return "";
    var diff = Date.now() - new Date(iso).getTime();
    if(diff < 60000) return Math.floor(diff/1000) + "s ago";
    if(diff < 3600000) return Math.floor(diff/60000) + "m ago";
    if(diff < 86400000) return Math.floor(diff/3600000) + "h ago";
    return Math.floor(diff/86400000) + "d ago";
  }
  function fmtTokens(n){
    if(n == null || n === 0) return "0";
    if(n >= 1000000) return (n/1000000).toFixed(1) + "M";
    if(n >= 1000) return (n/1000).toFixed(1) + "k";
    return String(n);
  }
  function statusClass(s){ return "status-" + (s || "unknown"); }
  function phaseClass(p){ return "phase-" + (p || "unknown"); }
  function issueStatusClass(s){
    if(!s) return "";
    return "issue-status-" + s.toLowerCase().replace(/\\s+/g, "-");
  }

  function renderSummary(data){
    var s = data.summary;
    var totalTokens = 0;
    var activeWorkers = 0;
    Object.keys(data.groups).forEach(function(repo){
      var issues = data.groups[repo];
      Object.keys(issues).forEach(function(num){
        issues[num].workers.forEach(function(w){
          if(w.activity && w.activity.tokensUsed) totalTokens += w.activity.tokensUsed;
          if(w.status === "running") activeWorkers++;
        });
      });
    });
    var html = "";
    html += '<div class="card"><div class="card-label">Total Workers</div>';
    html += '<div class="card-value">' + s.totalWorkers + '</div>';
    html += '<div class="card-detail">' + activeWorkers + ' active</div></div>';

    html += '<div class="card"><div class="card-label">Tokens Used</div>';
    html += '<div class="card-value">' + fmtTokens(totalTokens) + '</div></div>';

    var statusEntries = Object.keys(s.byStatus);
    if(statusEntries.length > 0){
      html += '<div class="card"><div class="card-label">By Status</div>';
      html += '<div class="card-detail">';
      statusEntries.forEach(function(k){
        html += '<span class="worker-status ' + statusClass(k) + '" style="margin-right:4px">';
        html += esc(k) + ': ' + s.byStatus[k] + '</span> ';
      });
      html += '</div></div>';
    }

    var phaseEntries = Object.keys(s.byPhase);
    if(phaseEntries.length > 0){
      html += '<div class="card"><div class="card-label">By Phase</div>';
      html += '<div class="card-detail">';
      phaseEntries.forEach(function(k){
        html += '<span class="worker-phase ' + phaseClass(k) + '" style="margin-right:4px">';
        html += esc(k) + ': ' + s.byPhase[k] + '</span> ';
      });
      html += '</div></div>';
    }

    $("summary").innerHTML = html;
  }

  function renderPipeline(data){
    var phaseCounts = {};
    PHASES.forEach(function(p){ phaseCounts[p] = 0; });
    Object.keys(data.groups).forEach(function(repo){
      var issues = data.groups[repo];
      Object.keys(issues).forEach(function(num){
        issues[num].workers.forEach(function(w){
          if(w.phase && phaseCounts[w.phase] !== undefined){
            phaseCounts[w.phase]++;
          }
        });
      });
    });
    var html = '<div class="pipeline">';
    PHASES.forEach(function(p, i){
      var count = phaseCounts[p];
      var cls = count > 0 ? " active" : "";
      html += '<div class="pipeline-stage' + cls + '">';
      html += esc(p) + (count > 0 ? '<span class="count">(' + count + ')</span>' : '');
      html += '</div>';
      if(i < PHASES.length - 1) html += '<span class="pipeline-arrow">&#9654;</span>';
    });
    html += '</div>';
    $("pipeline").innerHTML = html;
  }

  function renderWorker(w){
    var html = '<div class="worker">';
    html += '<div class="worker-info">';
    if(w.phase){
      html += '<span class="worker-phase ' + phaseClass(w.phase) + '">' + esc(w.phase) + '</span>';
    }
    html += '<span class="worker-status ' + statusClass(w.status) + '">' + esc(w.status) + '</span>';
    html += '<span class="worker-id" title="' + esc(w.id) + '">' + esc(w.id) + '</span>';
    if(w.crashCount > 0){
      html += '<span style="color:var(--red);font-size:0.75rem">crashes: ' + w.crashCount + '</span>';
    }
    html += '</div>';
    html += '<div class="worker-activity">';
    if(w.activity){
      html += '<span title="Tokens used">&#x1F4B0; ' + fmtTokens(w.activity.tokensUsed) + '</span>';
      html += '<span title="Messages">&#x1F4AC; ' + (w.activity.messageCount || 0) + '</span>';
      html += '<span title="Turns">&#x1F504; ' + (w.activity.turnCount || 0) + '</span>';
      if(w.activity.lastActivityAt){
        html += '<span title="Last active">' + relTime(w.activity.lastActivityAt) + '</span>';
      }
    }
    if(w.startedAt){
      html += '<span title="Started">started ' + relTime(w.startedAt) + '</span>';
    }
    html += '</div></div>';
    return html;
  }

  function renderGroups(data){
    var repos = Object.keys(data.groups);
    if(repos.length === 0){
      $("groups").innerHTML = '<div class="empty"><div class="empty-icon">&#x1F6B0;</div>' +
        '<div class="empty-text">No active workers</div></div>';
      return;
    }
    var html = "";
    repos.sort().forEach(function(repo){
      html += '<div class="repo-group">';
      html += '<div class="repo-header">' + esc(repo) + '</div>';
      var issues = data.groups[repo];
      var nums = Object.keys(issues).sort(function(a,b){ return Number(a) - Number(b); });
      nums.forEach(function(num){
        var issue = issues[num];
        html += '<div class="issue">';
        html += '<div class="issue-header">';
        html += '<span class="issue-number">#' + esc(num) + '</span>';
        if(issue.issueTitle){
          html += '<span class="issue-title">' + esc(issue.issueTitle) + '</span>';
        }
        if(issue.issueStatus){
          html += '<span class="issue-status ' + issueStatusClass(issue.issueStatus) + '">';
          html += esc(issue.issueStatus) + '</span>';
        }
        html += '</div>';
        issue.workers.forEach(function(w){
          html += renderWorker(w);
        });
        html += '</div>';
      });
      html += '</div>';
    });
    $("groups").innerHTML = html;
  }

  function renderEvents(data){
    var events = data.recentEvents || [];
    if(events.length === 0){
      $("events-section").innerHTML = "";
      return;
    }
    var html = '<div class="events-title' + (eventsOpen ? " open" : "") + '" id="events-toggle">';
    html += 'Recent Events (' + events.length + ')</div>';
    html += '<div class="events-list' + (eventsOpen ? " open" : "") + '" id="events-list">';
    events.forEach(function(e){
      html += '<div class="event-row">';
      html += '<span class="event-time">' + relTime(e.timestamp) + '</span>';
      html += '<span class="event-type">' + esc(e.event) + '</span>';
      html += '<span class="event-detail">' + esc(e.workerId || "") + '</span>';
      html += '</div>';
    });
    html += '</div>';
    $("events-section").innerHTML = html;
    var toggle = $("events-toggle");
    if(toggle){
      toggle.addEventListener("click", function(){
        eventsOpen = !eventsOpen;
        var list = $("events-list");
        toggle.className = "events-title" + (eventsOpen ? " open" : "");
        if(list) list.className = "events-list" + (eventsOpen ? " open" : "");
      });
    }
  }

  function render(data){
    renderSummary(data);
    renderPipeline(data);
    renderGroups(data);
    renderEvents(data);
    $("updated").textContent = "Updated " + new Date(data.generatedAt).toLocaleTimeString();
    $("error").className = "error-banner";
  }

  function showError(msg){
    var el = $("error");
    el.textContent = "Failed to fetch dashboard data: " + msg;
    el.className = "error-banner visible";
  }

  function fetchData(){
    var indicator = $("indicator");
    indicator.className = "refresh-indicator fetching";
    fetch("/dashboard")
      .then(function(res){
        if(!res.ok) throw new Error("HTTP " + res.status);
        return res.json();
      })
      .then(function(data){
        render(data);
        indicator.className = "refresh-indicator";
      })
      .catch(function(err){
        showError(err.message);
        indicator.className = "refresh-indicator";
      });
  }

  function tick(){
    countdown--;
    if(countdown <= 0){
      countdown = REFRESH_INTERVAL;
      fetchData();
    }
    $("countdown").textContent = String(countdown);
  }

  // Initial fetch
  fetchData();
  timer = setInterval(tick, 1000);
})();
</script>
</body>
</html>`;
}
