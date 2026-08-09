// Highlights the nav tab for the current page. Progressive enhancement only —
// the console is fully usable without it. Served same-origin so it satisfies
// the daemon's `script-src 'self'` CSP (no inline script needed).
(function () {
  var path = window.location.pathname;
  var links = document.querySelectorAll("nav a");
  for (var i = 0; i < links.length; i++) {
    var href = links[i].getAttribute("href");
    // "/ui/" (Status) matches only its exact path; every other tab also
    // matches its sub-paths (e.g. /ui/projects/foo/memories highlights Projects).
    // The memory edit page (/ui/memories/{id}/edit) is reached via the
    // Projects drill-in modal and has no owning nav tab, so it highlights none.
    if (href === path || (href !== "/ui/" && path.indexOf(href) === 0)) {
      links[i].classList.add("active");
    }
  }
})();
