package engine

// fakeHostGatedPage mimics Google Meet's host-restricted access screen. The
// gate lifts only from the second load of the same tab (sessionStorage counts
// them), simulating "the host has started the meeting" — so the bot has to
// re-request entry by reloading, exactly like against a real gate.
const fakeHostGatedPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="gate">You can't join this video call &nbsp; No one can join a meeting unless invited or admitted by the host &nbsp; Return to home screen</div>
  <div id="ready" style="display:none">
    <button id="present" aria-label="Present now">Present now</button>
  </div>
<script>
  var n = parseInt(sessionStorage.getItem('visits') || '0', 10) + 1;
  sessionStorage.setItem('visits', String(n));
  document.body.dataset.visits = n;
  if (n >= 2) {
    document.getElementById('gate').style.display = 'none';
    document.getElementById('ready').style.display = 'block';
  }
</script>
</body></html>`
