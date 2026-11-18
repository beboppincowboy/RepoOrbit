import { useEffect, useRef, useState } from "react";
import * as echarts from "echarts/core";
import { LineChart as LineSeries } from "echarts/charts";
import { GridComponent } from "echarts/components";
import { SVGRenderer } from "echarts/renderers";

echarts.use([LineSeries, GridComponent, SVGRenderer]);

// One line per series; null means the day is not covered (never drawn as zero).
export type ChartSeries = { name: string; unit: string; color: "--primary" | "--chart-2"; values: (number | null)[] };

const dayLabel = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
const token = (name: string) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

// Daily line chart styled like prototype D2's SVG: thin lines, light thirds grid, hatched leading days with no
// imported data, and a hover/focus day readout above the plot. Each day is a focusable column for keyboard use.
export function LineChart({ days, series }: { days: string[]; series: ChartSeries[] }) {
  const plot = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<number | null>(null);
  const n = days.length;
  const uncovered = (i: number) => series.every(s => s.values[i] === null);
  let hatched = 0;
  while (hatched < n && uncovered(hatched)) hatched++;

  useEffect(() => {
    const el = plot.current;
    if (!el) return;
    const chart = echarts.init(el, null, { renderer: "svg" });
    const draw = () => {
      const max = Math.max(1, ...series.flatMap(s => s.values.map(v => v ?? 0))) * 1.15;
      chart.setOption({
        animation: false,
        grid: { left: 0, right: 0, top: 4, bottom: 1 },
        xAxis: { type: "category", data: days, boundaryGap: true, axisLine: { lineStyle: { color: token("--border-strong") } }, axisTick: { show: false }, axisLabel: { show: false } },
        yAxis: { type: "value", min: 0, max, interval: max / 3, axisLabel: { show: false }, splitLine: { showMaxLine: false, lineStyle: { color: token("--border") } } },
        series: series.map(s => ({ type: "line", name: s.name, data: s.values, silent: true, showSymbol: false, lineStyle: { width: 2.2, color: token(s.color), join: "round" } })),
      }, true);
    };
    draw();
    const resize = new ResizeObserver(() => chart.resize());
    resize.observe(el);
    const scheme = matchMedia("(prefers-color-scheme: dark)");
    scheme.addEventListener("change", draw);
    return () => { resize.disconnect(); scheme.removeEventListener("change", draw); chart.dispose(); };
  }, [days, series]);

  return <>
    <div className="min-h-5 text-[12.5px]" aria-live="polite">
      {hover === null || hover >= n ? <span className="text-muted-foreground">Hover or focus the chart to read a day.</span> : <>
        <b className="font-semibold">{dayLabel.format(new Date(days[hover]))}</b> · {uncovered(hover)
          ? <span className="text-muted-foreground">not imported yet</span>
          : series.map(s => `${s.values[hover] ?? "—"} ${s.unit}`).join(" · ")}
      </>}
    </div>
    <div className="relative min-h-[120px] flex-1" onMouseLeave={() => setHover(null)}>
      {hatched > 0 && <div data-testid="chart-hatch" className="hatch absolute inset-y-0 left-0" style={{ width: `${hatched / n * 100}%` }} />}
      <div ref={plot} className="absolute inset-0" role="img" aria-label={`${series.map(s => s.name).join(" and ")} per day`} />
      {hover !== null && hover < n && <div className="pointer-events-none absolute inset-y-0 border-l border-dashed border-muted-foreground" style={{ left: `${(hover + 0.5) / n * 100}%` }} />}
      <div className="absolute inset-0 flex">
        {days.map((day, i) => <button key={day} type="button" aria-label={dayLabel.format(new Date(day))} className="min-w-0 flex-1 cursor-crosshair rounded-none focus-visible:outline-offset-[-2px]"
          onMouseEnter={() => setHover(i)} onFocus={() => setHover(i)} onBlur={() => setHover(null)} />)}
      </div>
    </div>
    <div className="flex justify-between font-mono text-xs text-muted-foreground">
      <span>{n ? dayLabel.format(new Date(days[0])) : ""}</span><span>{n ? dayLabel.format(new Date(days[n - 1])) : ""}</span>
    </div>
  </>;
}
