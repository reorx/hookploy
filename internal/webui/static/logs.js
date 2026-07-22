// NDJSON log streaming for .log-window[data-follow] elements.
//
// Two modes:
//   detail — full replay (?format=json for terminal deploys, ?follow=1
//            otherwise), op anchors, execution filter, pin-to-bottom toggle.
//   card   — dashboard in-progress cards: tail only, keeps the last 200
//            lines. Survives fragment polling: on hookploy:fragment the
//            freshly-rendered empty window is swapped for the live one.
//
// A dropped follow stream reconnects after 2s; follow always replays from
// the start, so the window is cleared before re-render (no dedupe needed).
(function () {
  var streams = new Map(); // deploy id → state
  var execMap = {}; // execution_id → {instance, ops:{index→name}}

  document.addEventListener('DOMContentLoaded', function () {
    var mapEl = document.getElementById('exec-map');
    if (mapEl) {
      try {
        execMap = JSON.parse(mapEl.textContent);
      } catch (e) {
        /* absent on non-detail pages */
      }
    }
    attachAll();
    initDetailControls();
  });
  document.addEventListener('hookploy:fragment', attachAll);

  function attachAll() {
    document.querySelectorAll('[data-follow]').forEach(function (el) {
      var live = streams.get(el.dataset.follow);
      if (live) {
        if (live.el !== el) el.replaceWith(live.el); // keep accumulated content
      } else {
        start(el);
      }
    });
    streams.forEach(function (s, id) {
      if (!document.contains(s.el)) {
        s.ctrl.abort();
        streams.delete(id);
      }
    });
  }

  function start(el) {
    var state = {
      el: el,
      ctrl: new AbortController(),
      mode: el.dataset.mode || 'card',
      map: mapFor(el),
      seen: new Set(), // (exec:op) groups that got an anchor id
      lastKey: null,
      count: 0,
      pinned: true,
      filter: '',
      done: false,
    };
    streams.set(el.dataset.follow, state);
    if (state.mode === 'detail') {
      el.addEventListener('scroll', function () {
        var nearBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 8;
        if (state.pinned !== nearBottom) setPinned(state, nearBottom);
      });
    }
    if (state.mode === 'detail' && el.dataset.terminal === 'true') {
      replay(state);
    } else {
      follow(state);
    }
  }

  async function replay(state) {
    try {
      var resp = await fetch('/deploys/' + state.el.dataset.follow + '/logs?format=json', {
        signal: state.ctrl.signal,
      });
      if (resp.ok) await readNDJSON(resp, state);
    } catch (e) {
      /* replay is one-shot; the page reload path recovers */
    }
  }

  async function follow(state) {
    var id = state.el.dataset.follow;
    while (!state.done && document.contains(state.el)) {
      try {
        var resp = await fetch('/deploys/' + id + '/logs?follow=1', { signal: state.ctrl.signal });
        if (resp.status === 401 || resp.status === 404) return;
        state.el.textContent = '';
        state.seen.clear();
        state.lastKey = null;
        state.count = 0;
        await readNDJSON(resp, state);
      } catch (e) {
        if (state.ctrl.signal.aborted) return;
      }
      if (!state.done) await sleep(2000);
    }
    if (state.done && state.mode === 'detail') refreshStatus(id);
  }

  async function readNDJSON(resp, state) {
    var reader = resp.body.getReader();
    var dec = new TextDecoder();
    var buf = '';
    for (;;) {
      var chunk = await reader.read();
      if (chunk.done) break;
      buf += dec.decode(chunk.value, { stream: true });
      var nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        var line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        if (!line.trim()) continue;
        var frame;
        try {
          frame = JSON.parse(line);
        } catch (e) {
          continue; // divergent frame: skip, keep the stream alive
        }
        if (frame.done) {
          state.done = true;
          return;
        }
        append(state, frame);
      }
    }
  }

  function append(state, frame) {
    var span = document.createElement('span');
    span.className = 'log-line ' + (frame.stream || 'stdout');
    span.dataset.exec = frame.execution_id;
    var key = frame.execution_id + ':' + frame.op_index;
    if (state.mode === 'detail' && !state.seen.has(key)) {
      state.seen.add(key);
      span.id = 'log-' + frame.execution_id + '-' + frame.op_index;
    }
    if (key !== state.lastKey) {
      state.lastKey = key;
      var p = document.createElement('span');
      p.className = 'prefix';
      p.textContent = '[' + prefixFor(state, frame) + '] ';
      span.appendChild(p);
    }
    span.appendChild(document.createTextNode(frame.data));
    if (state.mode === 'detail' && state.filter && frame.execution_id !== state.filter) {
      span.classList.add('hidden');
    }
    state.el.appendChild(span);
    state.count++;
    if (state.mode === 'card' && state.count > 200) {
      state.el.removeChild(state.el.firstChild);
      state.count--;
    }
    if (state.mode === 'card' || state.pinned) state.el.scrollTop = state.el.scrollHeight;
  }

  // mapFor resolves the exec map for a window: the page-level #exec-map
  // (detail pages) or the per-card #exec-map-<deploy-id> rendered next to
  // dashboard cards. Ops snapshots never change, so reading once is enough.
  function mapFor(el) {
    var cardMap = document.getElementById('exec-map-' + el.dataset.follow);
    if (cardMap) {
      try {
        return JSON.parse(cardMap.textContent);
      } catch (e) {
        /* fall through */
      }
    }
    return execMap;
  }

  function prefixFor(state, frame) {
    var entry = state.map[frame.execution_id];
    if (!entry) return frame.execution_id.slice(0, 8) + '/' + frame.op_index;
    var op = entry.ops[String(frame.op_index)] || frame.op_index;
    return entry.instance + '/' + op;
  }

  function setPinned(state, v) {
    state.pinned = v;
    var pin = document.getElementById('pin-bottom');
    if (pin) pin.checked = v;
  }

  function initDetailControls() {
    var viewer = document.getElementById('log-viewer');
    if (!viewer) return;
    var select = document.getElementById('exec-filter');
    var pin = document.getElementById('pin-bottom');
    select.addEventListener('change', function () {
      applyFilter(viewer, select.value);
    });
    pin.addEventListener('change', function () {
      var s = streams.get(viewer.dataset.follow);
      if (!s) return;
      s.pinned = pin.checked;
      if (pin.checked) viewer.scrollTop = viewer.scrollHeight;
    });
    // op rows live inside the polled status region: delegate the click
    document.addEventListener('click', function (ev) {
      var row = ev.target.closest('.op-row');
      if (!row) return;
      select.value = row.dataset.exec;
      applyFilter(viewer, row.dataset.exec);
      var anchor = document.getElementById('log-' + row.dataset.exec + '-' + row.dataset.op);
      if (anchor) {
        var s = streams.get(viewer.dataset.follow);
        if (s) setPinned(s, false);
        anchor.scrollIntoView({ block: 'start' });
      }
    });
  }

  function applyFilter(viewer, execId) {
    var s = streams.get(viewer.dataset.follow);
    if (s) s.filter = execId;
    viewer.querySelectorAll('.log-line').forEach(function (line) {
      line.classList.toggle('hidden', !!execId && line.dataset.exec !== execId);
    });
  }

  function refreshStatus(id) {
    var region = document.getElementById('deploy-status');
    if (!region) return;
    fetch('/ui/fragments/deploys/' + id + '/status')
      .then(function (resp) {
        return resp.ok ? resp.text() : null;
      })
      .then(function (html) {
        if (html) {
          region.removeAttribute('data-poll'); // deploy settled: stop polling
          region.innerHTML = html;
        }
      })
      .catch(function () {});
  }

  function sleep(ms) {
    return new Promise(function (r) {
      setTimeout(r, ms);
    });
  }
})();
