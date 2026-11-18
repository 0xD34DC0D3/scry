// Theme toggle. Loaded synchronously in <head> (no defer) so data-theme is set
// before first paint. No data-theme means "follow the OS"; the button cycles
// system -> light -> dark -> system so the user can always get back to it.
// localStorage can throw (private mode, blocked storage): never rely on it.
(function () {
  var KEY = "theme";
  var ORDER = ["system", "light", "dark"];
  var root = document.documentElement;

  function read() {
    try {
      var v = localStorage.getItem(KEY);
      return v === "light" || v === "dark" ? v : "system";
    } catch (e) {
      return "system";
    }
  }

  function write(v) {
    try {
      if (v === "system") localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, v);
    } catch (e) {}
  }

  function apply(v) {
    if (v === "system") delete root.dataset.theme;
    else root.dataset.theme = v;
  }

  var current = read();
  apply(current);

  document.addEventListener("DOMContentLoaded", function () {
    var btn = document.getElementById("theme-toggle");
    if (!btn) return;
    function render() {
      var next = ORDER[(ORDER.indexOf(current) + 1) % ORDER.length];
      btn.textContent = current;
      btn.setAttribute("aria-label", "Theme: " + current + ". Switch to " + next);
    }
    btn.addEventListener("click", function () {
      current = ORDER[(ORDER.indexOf(current) + 1) % ORDER.length];
      write(current);
      apply(current);
      render();
    });
    render();
    btn.hidden = false;
  });
})();
