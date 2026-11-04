(function () {
  var style = getComputedStyle(document.documentElement);
  var accent = style.getPropertyValue('--accent').trim();
  var accent2 = style.getPropertyValue('--accent2').trim();
  var ink = style.getPropertyValue('--ink').trim();
  var muted = style.getPropertyValue('--muted').trim();
  var rule = style.getPropertyValue('--rule').trim();

  var el = document.getElementById('chart-cases');
  if (!el || typeof echarts === 'undefined') return;

  var chart = echarts.init(el, null, { renderer: 'svg' });
  chart.setOption({
    animation: false,
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      appendToBody: true,
      formatter: function (p) {
        var d = p[0];
        return d.name + '<br/>通过用例：' + d.value + ' / ' + d.value + '（全部通过）';
      }
    },
    grid: { left: 110, right: 40, top: 20, bottom: 30 },
    xAxis: {
      type: 'value',
      minInterval: 1,
      axisLabel: { color: muted },
      splitLine: { lineStyle: { color: rule } }
    },
    yAxis: {
      type: 'category',
      inverse: true,
      data: ['HTTP 分块下载', '磁力链接', 'BT 种子', '抖音视频', 'm3u8 视频流', '附加功能'],
      axisLabel: { color: ink, fontSize: 12 },
      axisLine: { lineStyle: { color: rule } },
      axisTick: { show: false }
    },
    series: [{
      type: 'bar',
      data: [6, 4, 4, 3, 4, 6],
      barWidth: '55%',
      itemStyle: { color: accent2, borderRadius: [0, 4, 4, 0] },
      label: {
        show: true,
        position: 'right',
        color: ink,
        fontWeight: 600,
        formatter: '{c} ✓'
      }
    }]
  });
  window.addEventListener('resize', function () { chart.resize(); });
})();
