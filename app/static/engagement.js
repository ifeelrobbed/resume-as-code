(function () {
  // sendBeacon (not fetch) - guaranteed to actually send even though the
  // click immediately navigates the page away, unlike a normal fetch()
  // which can get cancelled mid-flight by the navigation.
  document.querySelectorAll('a[data-engagement]').forEach(function (el) {
    el.addEventListener('click', function () {
      navigator.sendBeacon('/engagement/click?target=' + encodeURIComponent(el.dataset.engagement));
    });
  });
})();
