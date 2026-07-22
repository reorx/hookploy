// Fragment poller: any element with data-poll="<url>" gets its innerHTML
// replaced with the server-rendered fragment every 3s. Rendering stays
// single-source (templ); this script only moves HTML. Polling pauses while
// the page is hidden. Log windows are managed by logs.js and are excluded
// from replacement by living outside data-poll containers (or being
// re-attached via the hookploy:fragment event).
(function () {
  var INTERVAL = 3000;

  async function pollOne(el) {
    try {
      var resp = await fetch(el.dataset.poll, { headers: { Accept: 'text/html' } });
      if (!resp.ok) return;
      var html = await resp.text();
      el.innerHTML = html;
      el.dispatchEvent(new CustomEvent('hookploy:fragment', { bubbles: true }));
    } catch (e) {
      /* transient network error: try again next tick */
    }
  }

  function tick() {
    if (document.hidden) return;
    document.querySelectorAll('[data-poll]').forEach(pollOne);
  }

  document.addEventListener('DOMContentLoaded', function () {
    var els = document.querySelectorAll('[data-poll]');
    if (!els.length) return;
    els.forEach(pollOne); // initial fill for empty containers
    setInterval(tick, INTERVAL);
  });
})();
