// Delta-V web UI: the little the pages need beyond htmx. Loaded in <head>,
// before the page paints, so a chosen theme is in place at once.
(function () {
  "use strict";

  // Theme. The stylesheet follows the system's light or dark setting on its
  // own; a choice made with the button is remembered in this browser and
  // overrides it.
  var root = document.documentElement;
  var KEY = "deltav-theme";
  try {
    var saved = localStorage.getItem(KEY);
    if (saved === "light" || saved === "dark") root.dataset.theme = saved;
  } catch (e) { /* storage unavailable: follow the system */ }

  function theme() {
    return root.dataset.theme ||
      (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  }

  document.addEventListener("click", function (event) {
    if (!event.target.closest || !event.target.closest("#theme")) return;
    var next = theme() === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try { localStorage.setItem(KEY, next); } catch (e) { /* not remembered */ }
  });

  // Whether the page is being kept up to date, in words beside the dot.
  var words = {
    live: "Live",
    connecting: "Connecting",
    lost: "Connection lost, retrying",
    off: "Not updating"
  };
  var state = "off";

  function show(next) {
    state = next;
    var box = document.getElementById("live-state");
    var text = document.getElementById("live-text");
    if (box) box.dataset.state = next;
    if (text) text.textContent = words[next];
  }

  document.addEventListener("htmx:sse:before:connection", function () {
    if (state !== "live") show("connecting");
  });
  document.addEventListener("htmx:sse:after:connection", function () { show("live"); });
  document.addEventListener("htmx:sse:after:message", function () { show("live"); });
  document.addEventListener("htmx:sse:error", function (event) {
    // The session ran out: the login page is the way back.
    if (event.detail && event.detail.status === 401) {
      window.location.assign("/login?next=" + encodeURIComponent(location.pathname + location.search));
      return;
    }
    show("lost");
  });
  // A page's main part is replaced when its parameters change, and the new
  // one starts out saying "Not updating" until its own stream is open.
  document.addEventListener("htmx:after:swap", function () { show(state); });
})();
