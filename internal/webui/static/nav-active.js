// Highlights the nav tab for the current page. Progressive enhancement only —
// the console is fully usable without it. Served same-origin so it satisfies
// the daemon's `script-src 'self'` CSP (no inline script needed).
(function () {
  var path = window.location.pathname;
  var links = document.querySelectorAll("nav a");
  for (var i = 0; i < links.length; i++) {
    var href = links[i].getAttribute("href");
    // "/ui/" (Status) matches only its exact path; every other tab also
    // matches its sub-paths (e.g. /ui/memories/5/edit highlights Memories).
    if (href === path || (href !== "/ui/" && path.indexOf(href) === 0)) {
      links[i].classList.add("active");
    }
  }
})();
