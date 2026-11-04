/* Xlauch 下载器官网 - 交互脚本 */
(function () {
  "use strict";

  // 导航栏滚动状态 + 返回顶部按钮
  var navbar = document.getElementById("navbar");
  var backtop = document.getElementById("backtop");
  function onScroll() {
    if (window.scrollY > 8) navbar.classList.add("scrolled");
    else navbar.classList.remove("scrolled");
    if (window.scrollY > 480) backtop.classList.add("show");
    else backtop.classList.remove("show");
  }
  window.addEventListener("scroll", onScroll, { passive: true });
  onScroll();
  backtop.addEventListener("click", function () {
    window.scrollTo({ top: 0, behavior: "smooth" });
  });

  // 滚动显现动画（无 IntersectionObserver 时直接显示）
  if ("IntersectionObserver" in window) {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting) {
          en.target.classList.add("reveal-in");
          io.unobserve(en.target);
        }
      });
    }, { threshold: 0.12 });
    document.querySelectorAll(".fcard, .step, .stat, .tip, .dl-card, .faq-list details").forEach(function (el) {
      el.classList.add("reveal");
      io.observe(el);
    });
  }

  // 平滑滚动补偿固定导航高度
  document.querySelectorAll('a[href^="#"]').forEach(function (a) {
    a.addEventListener("click", function (e) {
      var id = a.getAttribute("href");
      if (id.length < 2) return;
      var target = document.querySelector(id);
      if (!target) return;
      e.preventDefault();
      var top = target.getBoundingClientRect().top + window.scrollY - 76;
      window.scrollTo({ top: top, behavior: "smooth" });
    });
  });
})();
