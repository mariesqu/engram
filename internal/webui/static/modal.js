// Project drill-in modal open/close behavior. The modal markup itself is
// inserted into/removed from #modal-root entirely by htmx swaps (see
// project_memories.html and projects.html), so these listeners are
// delegated on document rather than bound to elements directly — they keep
// working across every swap without re-binding. Served same-origin so it
// satisfies the daemon's `script-src 'self'` CSP (no inline script needed).
(function () {
  // suppressSwap guards against the escape/close race: closeModal() clears
  // #modal-root synchronously, but an htmx GET that was already in flight
  // when the user closed the modal (e.g. Escape pressed mid-fetch, or a
  // backdrop click right after opening) can still resolve afterwards and
  // swap the modal right back in. Once set, any swap INTO #modal-root is
  // blocked until a NEW request targeting #modal-root starts —
  // htmx:beforeRequest clears the flag immediately (before that request's
  // own response can arrive), so opening the modal again right after a
  // close still works normally.
  var suppressSwap = false;

  function closeModal() {
    var root = document.getElementById("modal-root");
    suppressSwap = true;
    if (root) {
      root.innerHTML = "";
    }
  }

  document.addEventListener("click", function (e) {
    var closeTrigger = e.target.closest && e.target.closest("[data-modal-close]");
    if (closeTrigger) {
      e.preventDefault();
      closeModal();
      return;
    }
    // A click directly on the backdrop (not on the panel it wraps) also closes.
    if (e.target.hasAttribute && e.target.hasAttribute("data-modal-open")) {
      closeModal();
    }
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") {
      closeModal();
    }
  });

  document.body.addEventListener("htmx:beforeRequest", function (e) {
    if (e.detail && e.detail.target && e.detail.target.id === "modal-root") {
      suppressSwap = false;
    }
  });

  document.body.addEventListener("htmx:beforeSwap", function (e) {
    if (suppressSwap && e.detail && e.detail.target && e.detail.target.id === "modal-root") {
      e.detail.shouldSwap = false;
    }
  });

  // Minimal a11y: move focus into the modal panel whenever it is swapped
  // into #modal-root, so keyboard/screen-reader users land inside the
  // dialog instead of on whatever was focused on the underlying page. Full
  // focus trap / inert background is out of scope (see review notes) — the
  // panel itself carries tabindex="-1" so it is focusable as a target.
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (e.target && e.target.id === "modal-root") {
      var panel = e.target.querySelector(".modal-panel");
      if (panel && typeof panel.focus === "function") {
        panel.focus();
      }
    }
  });
})();
