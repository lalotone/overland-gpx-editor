import { useEffect, useState } from 'react'
import { altitudeColor } from '../lib/terrain'

/*
 * Intro animation: a topographic map drawing itself.
 *
 * Everything here is generated geometry — no tiles, no network. The previous
 * version pulled basemap tiles from a CDN, so with no connection (which is
 * most of the time this app is useful) it showed a blank dark screen.
 *
 * Contour rings use the app's own hypsometric ramp, so the first thing you
 * see is the colour language the elevation profile and altitude mode use.
 */

const VIEW_W = 420
const VIEW_H = 280

interface Peak {
  cx: number
  cy: number
  rings: number
  innerR: number
  step: number
  /** [frequency, amplitude] pairs that make a ring lumpy rather than round. */
  harmonics: [number, number][]
  squash: number
}

const PEAKS: Peak[] = [
  {
    cx: 140, cy: 130, rings: 7, innerR: 16, step: 15,
    harmonics: [[3, 0.13], [5, 0.07], [7, 0.04]],
    squash: 0.74,
  },
  {
    cx: 296, cy: 170, rings: 5, innerR: 13, step: 13,
    harmonics: [[2, 0.16], [4, 0.09], [6, 0.05]],
    squash: 0.7,
  },
  {
    cx: 352, cy: 74, rings: 4, innerR: 12, step: 12,
    harmonics: [[3, 0.18], [5, 0.08]],
    squash: 0.72,
  },
]

function ringPath(peak: Peak, radius: number): string {
  const STEPS = 84
  const points: string[] = []
  for (let i = 0; i <= STEPS; i++) {
    const t = (i / STEPS) * Math.PI * 2
    let r = radius
    for (const [freq, amp] of peak.harmonics) r += radius * amp * Math.sin(freq * t + freq)
    const x = peak.cx + Math.cos(t) * r
    const y = peak.cy + Math.sin(t) * r * peak.squash
    points.push(`${x.toFixed(1)},${y.toFixed(1)}`)
  }
  return `M${points.join('L')}Z`
}

interface Ring {
  d: string
  color: string
  delay: number
  width: number
}

/** Rings are emitted outermost first so the map fills inward, like a survey. */
const RINGS: Ring[] = PEAKS.flatMap((peak, peakIdx) =>
  Array.from({ length: peak.rings }, (_, i) => {
    const level = peak.rings - 1 - i // 0 = outermost
    const radius = peak.innerR + level * peak.step
    // Innermost ring is the summit, so it takes the top of the ramp.
    const t = (peak.rings - 1 - level) / Math.max(1, peak.rings - 1)
    return {
      d: ringPath(peak, radius),
      color: altitudeColor(t, 0, 1),
      delay: 0.08 + peakIdx * 0.12 + i * 0.055,
      width: level === 0 ? 1.5 : 1,
    }
  }),
)

/** The track: a hand-placed line that threads between the two summits. */
const TRACK_POINTS: [number, number][] = [
  [8, 232], [32, 226], [56, 228], [80, 220], [104, 214], [128, 208],
  [152, 202], [178, 194], [200, 180], [218, 162], [232, 146], [248, 136],
  [268, 130], [288, 126], [308, 124], [330, 122], [352, 132], [374, 148],
  [392, 160], [404, 168],
]

const TRACK_PATH = `M${TRACK_POINTS.map(([x, y]) => `${x},${y}`).join('L')}`

const TRACK_LENGTH = TRACK_POINTS.reduce((len, p, i) => {
  if (i === 0) return 0
  const dx = p[0] - TRACK_POINTS[i - 1][0]
  const dy = p[1] - TRACK_POINTS[i - 1][1]
  return len + Math.sqrt(dx * dx + dy * dy)
}, 0)

const TRACK_END = TRACK_POINTS[TRACK_POINTS.length - 1]

/** Elevation silhouette along the bottom edge. */
const PROFILE_PATH = (() => {
  const N = 72
  const baseline = VIEW_H
  const points: string[] = []
  for (let i = 0; i <= N; i++) {
    const t = i / N
    const height =
      0.34 +
      0.3 * Math.sin(t * 7.5 + 0.4) +
      0.17 * Math.sin(t * 17 + 1.2) +
      0.09 * Math.sin(t * 31 + 2.4)
    points.push(`${(t * VIEW_W).toFixed(1)},${(baseline - height * 42).toFixed(1)}`)
  }
  return `M0,${baseline}L${points.join('L')}L${VIEW_W},${baseline}Z`
})()

/** Total run before the splash dismisses itself, in ms. */
const HOLD_MS = 1900
const FADE_MS = 480

export function SplashScreen({ onDone }: { onDone: () => void }) {
  const [fading, setFading] = useState(false)

  useEffect(() => {
    const fade = setTimeout(() => setFading(true), HOLD_MS)
    const done = setTimeout(onDone, HOLD_MS + FADE_MS)
    return () => { clearTimeout(fade); clearTimeout(done) }
  }, [onDone])

  // Respect a reduced-motion preference by skipping straight to the app.
  useEffect(() => {
    if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) onDone()
  }, [onDone])

  return (
    <div
      className={`splash${fading ? ' splash--fading' : ''}`}
      onClick={onDone}
      role="button"
      tabIndex={0}
      aria-label="Skip intro"
      onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') onDone() }}
    >
      <div className="splash-stage">
        <svg
          className="splash-svg"
          viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
          preserveAspectRatio="xMidYMid meet"
          aria-hidden="true"
        >
          <defs>
            <linearGradient id="splash-track-grad" x1="0" y1="0" x2="1" y2="0">
              <stop offset="0%" stopColor="#38bdf8" />
              <stop offset="100%" stopColor="#818cf8" />
            </linearGradient>
            <linearGradient id="splash-profile-grad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="rgba(56,189,248,0.30)" />
              <stop offset="100%" stopColor="rgba(56,189,248,0.02)" />
            </linearGradient>
          </defs>

          {/* Graticule */}
          <g className="splash-grid">
            {Array.from({ length: 7 }, (_, i) => (
              <line key={`v${i}`} x1={(i + 1) * 52.5} y1="0" x2={(i + 1) * 52.5} y2={VIEW_H} />
            ))}
            {Array.from({ length: 4 }, (_, i) => (
              <line key={`h${i}`} x1="0" y1={(i + 1) * 56} x2={VIEW_W} y2={(i + 1) * 56} />
            ))}
          </g>

          {/* Contour rings drawing themselves */}
          {RINGS.map((ring, i) => (
            <path
              key={i}
              className="splash-contour"
              d={ring.d}
              stroke={ring.color}
              strokeWidth={ring.width}
              style={{ animationDelay: `${ring.delay}s` }}
            />
          ))}

          {/* Elevation silhouette */}
          <path className="splash-profile" d={PROFILE_PATH} fill="url(#splash-profile-grad)" />

          {/* The route */}
          <path
            className="splash-track"
            d={TRACK_PATH}
            style={{ '--track-len': TRACK_LENGTH.toFixed(0) } as React.CSSProperties}
          />

          {/* Destination marker */}
          <circle className="splash-track-halo" cx={TRACK_END[0]} cy={TRACK_END[1]} r="4" />
          <circle className="splash-track-dot" cx={TRACK_END[0]} cy={TRACK_END[1]} r="3.4" />
        </svg>
      </div>

      <div className="splash-brand">
        <div className="splash-title">
          <span className="splash-title-mark">GPX</span>
          <span className="splash-title-rest">Editor</span>
        </div>
        <p className="splash-tagline">Plan · Ride · Map the dirt</p>
      </div>

      <span className="splash-skip">Click to skip</span>
    </div>
  )
}
