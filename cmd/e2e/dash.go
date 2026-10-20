package main

// A live view of the end-to-end run, served over HTTP.
//
// This exists because a terminal is hard to read from across a room. It shows
// exactly what the text output shows and adds nothing the system does not
// already know -- it is a window onto the same state, not a second source of
// truth. Every number on the page comes from the same variables the summary at
// the end is computed from.
//
// It is off unless -http is passed, so the demo that has been rehearsed in a
// terminal keeps working untouched.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"adlmf/internal/types"
)

type svcView struct {
	Name     string   `json:"name"`
	PID      int      `json:"pid"`
	State    string   `json:"state"` // running | holding | waiting | deadlocked | aborted | dead
	Held     []string `json:"held"`
	WaitFor  string   `json:"waitFor"`  // service it is blocked behind, if any
	WaitLock string   `json:"waitLock"` // the lock it is blocked on
	Aborts   int      `json:"aborts"`
}

type edgeView struct {
	From string `json:"from"`
	To   string `json:"to"`
	Lock string `json:"lock"`
}

type logLine struct {
	MS   int64  `json:"ms"`
	Kind string `json:"kind"` // flag | deadlock | kill | reclaim | info
	Text string `json:"text"`
}

type dashState struct {
	ElapsedMS  int64             `json:"elapsedMs"`
	TotalMS    int64             `json:"totalMs"`
	Endpoint   string            `json:"endpoint"`
	Prefix     string            `json:"prefix"`
	Done       bool              `json:"done"`
	Services   []svcView         `json:"services"`
	Locks      map[string]string `json:"locks"`
	Edges      []edgeView        `json:"edges"`
	Events     int               `json:"events"`
	Deadlocks  int               `json:"deadlocks"`
	Flags      int               `json:"flags"`
	Violations int               `json:"violations"`
	KilledSvc  string            `json:"killedSvc"`
	KilledPID  int               `json:"killedPid"`
	ReclaimMS  int64             `json:"reclaimMs"`
	Log        []logLine         `json:"log"`
}

type dashboard struct {
	mu    sync.Mutex
	state dashState
	start time.Time

	// deadlockUntil holds the red "in deadlock" state on screen briefly after a
	// cycle is detected. Without it the state is essentially invisible: the
	// engine detects and breaks a deadlock inside one 50ms tick, so a viewer
	// never sees it. This shows something that genuinely happened, a moment
	// after it happened -- it does not invent a state the system was never in.
	deadlockUntil map[types.ServiceID]time.Time
}

func newDashboard(endpoint, prefix string, total time.Duration) *dashboard {
	return &dashboard{
		state: dashState{
			Endpoint: endpoint,
			Prefix:   prefix,
			TotalMS:  total.Milliseconds(),
			Locks:    map[string]string{},
		},
		start:         time.Now(),
		deadlockUntil: map[types.ServiceID]time.Time{},
	}
}

// serve starts the HTTP listener. Failure is reported and ignored: a dashboard
// that will not bind must never take the demo down with it.
func (d *dashboard) serve(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		s := d.state
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashHTML))
	})
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Printf("  (dashboard unavailable: %v)\n", err)
		}
	}()
}

func (d *dashboard) log(kind, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state.Log = append([]logLine{{
		MS:   time.Since(d.start).Milliseconds(),
		Kind: kind,
		Text: text,
	}}, d.state.Log...)
	if len(d.state.Log) > 60 {
		d.state.Log = d.state.Log[:60]
	}
}

type snapshotIn struct {
	stubs      map[types.ServiceID]int // live services -> pid
	dead       types.ServiceID
	deadPID    int
	held       map[types.ServiceID][]string
	blocked    map[types.ServiceID]edgeView
	inCycle    map[types.ServiceID]bool
	aborts     map[types.ServiceID]int
	locks      map[string]string
	events     int
	deadlocks  int
	flags      int
	violations int
	reclaimMS  int64
	done       bool
}

func (d *dashboard) update(in snapshotIn) {
	d.mu.Lock()
	now := time.Now()
	for id := range in.inCycle {
		d.deadlockUntil[id] = now.Add(1500 * time.Millisecond)
	}
	stillRed := map[types.ServiceID]bool{}
	for id, t := range d.deadlockUntil {
		if now.Before(t) {
			stillRed[id] = true
		}
	}
	d.mu.Unlock()

	var svcs []svcView
	names := make([]types.ServiceID, 0, len(in.stubs))
	for id := range in.stubs {
		names = append(names, id)
	}
	if in.dead != "" {
		names = append(names, in.dead)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	for _, id := range names {
		v := svcView{Name: string(id), Aborts: in.aborts[id]}
		if id == in.dead {
			v.PID, v.State = in.deadPID, "dead"
			svcs = append(svcs, v)
			continue
		}
		v.PID = in.stubs[id]
		v.Held = in.held[id]
		if e, ok := in.blocked[id]; ok {
			v.WaitFor, v.WaitLock = e.To, e.Lock
		}
		switch {
		case stillRed[id]:
			v.State = "deadlocked"
		case v.WaitFor != "":
			v.State = "waiting"
		case len(v.Held) > 0:
			v.State = "holding"
		default:
			v.State = "running"
		}
		svcs = append(svcs, v)
	}

	var edges []edgeView
	for _, e := range in.blocked {
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].From < edges[j].From })

	d.mu.Lock()
	d.state.ElapsedMS = time.Since(d.start).Milliseconds()
	d.state.Services = svcs
	d.state.Edges = edges
	d.state.Locks = in.locks
	d.state.Events = in.events
	d.state.Deadlocks = in.deadlocks
	d.state.Flags = in.flags
	d.state.Violations = in.violations
	d.state.ReclaimMS = in.reclaimMS
	d.state.Done = in.done
	if in.dead != "" {
		d.state.KilledSvc, d.state.KilledPID = string(in.dead), in.deadPID
	}
	d.mu.Unlock()
}

// finish marks the run complete and writes the final counters, deliberately
// WITHOUT touching services, locks or edges.
//
// The page is most looked at after the run ends -- that is when a panel reads
// it -- so blanking the live state at the finish line would leave them staring
// at an empty board. The last observed state is the right thing to leave up.
func (d *dashboard) finish(events, deadlocks, flags, violations int, reclaim time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state.Events = events
	d.state.Deadlocks = deadlocks
	d.state.Flags = flags
	d.state.Violations = violations
	d.state.ReclaimMS = reclaim.Milliseconds()
	d.state.ElapsedMS = time.Since(d.start).Milliseconds()
	d.state.Done = true
}

const dashHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Lock Manager - live</title>
<style>
:root {
  color-scheme: light;
  --plane:      #f9f9f7;
  --surface:    #fcfcfb;
  --ink:        #0b0b0b;
  --ink-2:      #52514e;
  --muted:      #898781;
  --rule:       #e1e0d9;
  --hair:       rgba(11,11,11,0.10);
  --good:       #0ca30c;
  --warning:    #fab219;
  --serious:    #ec835a;
  --critical:   #d03b3b;
  --accent:     #2a78d6;
}
@media (prefers-color-scheme: dark) {
  :root:where(:not([data-theme="light"])) {
    color-scheme: dark;
    --plane:   #0d0d0d;
    --surface: #1a1a19;
    --ink:     #ffffff;
    --ink-2:   #c3c2b7;
    --muted:   #898781;
    --rule:    #2c2c2a;
    --hair:    rgba(255,255,255,0.10);
    --accent:  #3987e5;
  }
}
:root[data-theme="dark"] {
  color-scheme: dark;
  --plane: #0d0d0d; --surface: #1a1a19; --ink: #fff; --ink-2: #c3c2b7;
  --rule: #2c2c2a; --hair: rgba(255,255,255,0.10); --accent: #3987e5;
}
* { box-sizing: border-box; }
body {
  margin: 0; padding: 24px 16px 40px;
  background: var(--plane); color: var(--ink);
  font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
}
.wrap { max-width: 1180px; margin: 0 auto; }
header { display: flex; flex-wrap: wrap; gap: 12px 24px; align-items: baseline;
  margin-bottom: 20px; }
h1 { font-size: 20px; margin: 0; font-weight: 650; letter-spacing: -0.01em; }
.sub { color: var(--muted); font-size: 13px; }
.clock { margin-left: auto; font-size: 13px; color: var(--ink-2);
  font-variant-numeric: tabular-nums; }
.theme { font: inherit; font-size: 12px; color: var(--ink-2); cursor: pointer;
  background: var(--surface); border: 1px solid var(--hair); border-radius: 6px;
  padding: 3px 10px; }
.theme:hover { color: var(--ink); }

.kpis { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px;
  margin-bottom: 20px; }
.kpi { background: var(--surface); border: 1px solid var(--hair);
  border-radius: 10px; padding: 14px 16px; }
.kpi .label { font-size: 12px; color: var(--muted); margin-bottom: 4px; }
.kpi .value { font-size: 30px; font-weight: 650; line-height: 1.1;
  letter-spacing: -0.02em; }
.kpi .note { font-size: 12px; color: var(--ink-2); margin-top: 2px; }
.kpi.ok .value { color: var(--good); }
.kpi.bad .value { color: var(--critical); }

.cols { display: grid; grid-template-columns: 1.35fr 1fr; gap: 16px; }
.panel { background: var(--surface); border: 1px solid var(--hair);
  border-radius: 10px; padding: 14px 16px; }
.panel h2 { font-size: 13px; font-weight: 600; margin: 0 0 12px;
  color: var(--ink-2); text-transform: uppercase; letter-spacing: 0.05em; }

.svc { display: flex; align-items: center; gap: 10px; padding: 9px 0;
  border-bottom: 1px solid var(--rule); }
.svc:last-child { border-bottom: 0; }
.dot { width: 10px; height: 10px; border-radius: 50%; flex: none; }
.svc .name { font-weight: 600; min-width: 92px; white-space: nowrap; flex: none; }
.svc .pid { color: var(--muted); font-size: 12px; white-space: nowrap;
  font-variant-numeric: tabular-nums; min-width: 68px; flex: none; }
.svc .state { font-size: 13px; min-width: 112px; white-space: nowrap; flex: none; }
.svc .detail { color: var(--ink-2); font-size: 13px; min-width: 0; flex: 1 1 auto; }
.chip { display: inline-block; border: 1px solid var(--hair); border-radius: 5px;
  padding: 1px 6px; font-size: 12px; margin-right: 4px; color: var(--ink-2); }

.lock { display: flex; align-items: baseline; gap: 10px; padding: 9px 0;
  border-bottom: 1px solid var(--rule); }
.lock:last-child { border-bottom: 0; }
.lock .id { font-weight: 600; min-width: 36px; }
.lock .holder { color: var(--ink-2); }
.lock .free { color: var(--muted); }

.edges { margin-top: 16px; }
.edge { font-size: 13px; color: var(--ink-2); padding: 4px 0; }
.edge b { color: var(--ink); font-weight: 600; }
.none { color: var(--muted); font-size: 13px; }

.logtitle { margin-top: 18px !important; }
.log {
  max-height: 300px;
  overflow-y: auto;
  /* New entries are PREPENDED. Without this, Chrome's scroll anchoring nudges
     the scroll position down to keep the visible content from jumping -- which
     pushes the newest entry, the one that matters, out of sight. Turning the
     anchoring off keeps the box scrollable while leaving the top at the top. */
  overflow-anchor: none;
}
.line { display: flex; gap: 10px; padding: 4px 0; font-size: 13px;
  border-bottom: 1px solid var(--rule); }
.line:last-child { border-bottom: 0; }
.hint { font-weight: 400; text-transform: none; letter-spacing: 0;
  color: var(--muted); font-size: 11px; }
.line .t { color: var(--muted); font-variant-numeric: tabular-nums;
  min-width: 62px; text-align: right; }
.line .ico { width: 16px; text-align: center; flex: none; }
.banner { margin-top: 16px; padding: 10px 14px; border-radius: 8px;
  border: 1px solid var(--hair); background: var(--surface); font-size: 14px; }
@media (max-width: 860px) {
  .kpis { grid-template-columns: repeat(2, 1fr); }
  .cols { grid-template-columns: 1fr; }
}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>Adaptive Distributed Lock Manager</h1>
    <span class="sub" id="ep"></span>
    <span class="clock" id="clock"></span>
    <button class="theme" id="theme" type="button" aria-label="Switch theme">Dark</button>
  </header>

  <div class="kpis">
    <div class="kpi"><div class="label">Lock events observed</div>
      <div class="value" id="k-events">0</div>
      <div class="note">from separate OS processes</div></div>
    <div class="kpi"><div class="label">Deadlocks broken</div>
      <div class="value" id="k-dead">0</div>
      <div class="note">victim signalled across processes</div></div>
    <div class="kpi"><div class="label">Pillar 1 warnings</div>
      <div class="value" id="k-flags">0</div>
      <div class="note">raised before anyone blocked</div></div>
    <div class="kpi ok" id="kpi-viol"><div class="label">Mutual-exclusion violations</div>
      <div class="value" id="k-viol">0</div>
      <div class="note" id="k-viol-note">checked against etcd every 50ms</div></div>
  </div>

  <div class="cols">
    <div class="panel">
      <h2>Services</h2>
      <div id="svcs"></div>
      <div class="edges"><h2>Waiting for</h2><div id="edges"></div></div>
    </div>
    <div class="panel">
      <h2>Locks in etcd</h2>
      <div id="locks"></div>
      <h2 class="logtitle">Event log <span class="hint" id="loghint"></span></h2>
      <div class="log"><div id="log"></div></div>
    </div>
  </div>

  <div class="banner" id="banner" style="display:none"></div>
</div>

<script>
// Status colour never carries meaning alone: every state ships an icon and a
// word as well, which is what makes the page readable in greyscale, on a
// projector, and to a colourblind viewer.
var STATE = {
  running:    { c: 'var(--good)',     i: '▶', t: 'running' },
  holding:    { c: 'var(--accent)',   i: '●', t: 'holding lock' },
  waiting:    { c: 'var(--warning)',  i: '⏸', t: 'waiting' },
  deadlocked: { c: 'var(--critical)', i: '⚠', t: 'DEADLOCKED' },
  aborted:    { c: 'var(--serious)',  i: '↺', t: 'aborted, retrying' },
  dead:       { c: 'var(--muted)',    i: '✕', t: 'killed (-9)' }
};
var ICON = { flag: '⚠', deadlock: '✖', kill: '✕',
             reclaim: '↻', info: '·' };
var COLOR = { flag: 'var(--warning)', deadlock: 'var(--critical)',
              kill: 'var(--ink)', reclaim: 'var(--good)', info: 'var(--muted)' };

function esc(s) {
  return String(s).replace(/[&<>"]/g, function (c) {
    return { '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;' }[c];
  });
}
function secs(ms) { return (ms / 1000).toFixed(1) + 's'; }

function draw(s) {
  document.getElementById('ep').textContent = s.endpoint + '  ·  ' + s.prefix;
  document.getElementById('clock').textContent =
    s.done ? 'finished at ' + secs(s.elapsedMs)
           : secs(s.elapsedMs) + ' / ' + secs(s.totalMs);

  document.getElementById('k-events').textContent = s.events;
  document.getElementById('k-dead').textContent = s.deadlocks;
  document.getElementById('k-flags').textContent = s.flags;
  document.getElementById('k-viol').textContent = s.violations;
  var kv = document.getElementById('kpi-viol');
  kv.className = 'kpi ' + (s.violations === 0 ? 'ok' : 'bad');

  var html = '';
  (s.services || []).forEach(function (v) {
    var st = STATE[v.state] || STATE.running;
    var detail = '';
    if (v.state === 'dead') {
      detail = 'process destroyed while holding a lock';
    } else {
      // Holding AND waiting is the shape of a deadlock, so both halves must be
      // on screen together -- showing only the waiting half hides the point.
      if (v.held && v.held.length) {
        detail += 'holds ' + v.held.map(function (l) {
          return '<span class="chip">' + esc(l) + '</span>';
        }).join('');
      }
      if (v.waitFor) {
        detail += (detail ? ' \u00B7 ' : '') + 'waiting on ' + esc(v.waitLock) +
                  ', held by <b>' + esc(v.waitFor) + '</b>';
      }
    }
    if (v.aborts) {
      detail += ' <span class="chip">aborted ' + v.aborts + '×</span>';
    }
    html += '<div class="svc">' +
      '<span class="dot" style="background:' + st.c + '"></span>' +
      '<span class="name">' + esc(v.name) + '</span>' +
      '<span class="pid">pid ' + (v.pid || '—') + '</span>' +
      '<span class="state" style="color:' + st.c + '">' + st.i + ' ' + st.t + '</span>' +
      '<span class="detail">' + detail + '</span></div>';
  });
  document.getElementById('svcs').innerHTML = html || '<div class="none">starting…</div>';

  var e = '';
  (s.edges || []).forEach(function (x) {
    e += '<div class="edge"><b>' + esc(x.from) + '</b> is waiting for <b>' +
         esc(x.to) + '</b> to release ' + esc(x.lock) + '</div>';
  });
  document.getElementById('edges').innerHTML = e ||
    '<div class="none">nobody is blocked</div>';

  var l = '';
  Object.keys(s.locks || {}).sort().forEach(function (k) {
    var h = s.locks[k];
    l += '<div class="lock"><span class="id">' + esc(k) + '</span>' +
      (h ? '<span class="holder">held by <b>' + esc(h) + '</b></span>'
         : '<span class="free">free</span>') + '</div>';
  });
  document.getElementById('locks').innerHTML = l || '<div class="none">—</div>';

  var all = s.log || [];
  var shown = all.slice(0, 40);
  document.getElementById('loghint').textContent =
    all.length > 6 ? '(newest first - scroll for more)' : '';
  var g = '';
  shown.forEach(function (x) {
    g += '<div class="line"><span class="t">' + secs(x.ms) + '</span>' +
      '<span class="ico" style="color:' + (COLOR[x.kind] || '') + '">' +
      (ICON[x.kind] || '') + '</span>' +
      '<span>' + esc(x.text) + '</span></div>';
  });
  document.getElementById('log').innerHTML = g ||
    '<div class="none">waiting for events…</div>';

  var b = document.getElementById('banner');
  if (s.done) {
    b.style.display = '';
    b.innerHTML = s.violations === 0
      ? '<b style="color:var(--good)">✓ PASS</b> — exclusion held across ' +
        'real processes throughout. After <b>kill -9</b> of ' + esc(s.killedSvc) +
        ' (pid ' + s.killedPid + '), the lock was reclaimed in <b>' +
        secs(s.reclaimMs) + '</b> by lease expiry — nothing in our code ' +
        'detected the crash.'
      : '<b style="color:var(--critical)">✗ FAIL</b> — ' +
        s.violations + ' mutual-exclusion violation(s).';
  }
}

var themeBtn = document.getElementById('theme');
function applyTheme(t) {
  document.documentElement.setAttribute('data-theme', t);
  themeBtn.textContent = t === 'dark' ? 'Light' : 'Dark';
  try { localStorage.setItem('adlmf-theme', t); } catch (e) {}
}
// ?theme=dark / ?theme=light forces a theme, which is handy for a projector
// and for checking both modes without touching OS settings.
var forced = (location.search.match(/[?&]theme=(dark|light)/) || [])[1];
var saved = null;
try { saved = localStorage.getItem('adlmf-theme'); } catch (e) {}
applyTheme(forced || saved || (window.matchMedia &&
  window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
themeBtn.addEventListener('click', function () {
  applyTheme(document.documentElement.getAttribute('data-theme') === 'dark'
    ? 'light' : 'dark');
});

function poll() {
  fetch('/state', { cache: 'no-store' })
    .then(function (r) { return r.json(); })
    .then(draw)
    .catch(function () {});
}
poll();
setInterval(poll, 250);
</script>
</body>
</html>`
