(function () {
  // sendBeacon (not fetch) - guaranteed to actually send even though the
  // click immediately navigates the page away, unlike a normal fetch()
  // which can get cancelled mid-flight by the navigation.
  function track(el) {
    navigator.sendBeacon('/engagement/click?target=' + encodeURIComponent(el.dataset.engagement));
  }
  document.querySelectorAll('a[data-engagement]').forEach(function (el) {
    // Left-click, and Ctrl/Cmd+click (open in new tab via the primary
    // button), both still fire 'click'. Middle-click ("open in new tab"
    // via the middle button) fires a *different* event, 'auxclick', not
    // 'click' - without this second listener, middle-clicking a link is
    // invisible to tracking entirely (confirmed missing in production
    // 2026-08-17). Right-click -> "Open in new tab" from the native
    // context menu isn't fixable the same way - no JS-observable event
    // fires for that path in any browser.
    el.addEventListener('click', function () { track(el); });
    el.addEventListener('auxclick', function (e) {
      if (e.button === 1) track(el);
    });
  });
})();
