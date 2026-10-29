/*
 * A small, consistent icon set for map features. Emoji render differently on
 * every platform and at marker size read as clip-art; these share one 24px
 * grid and one stroke weight, so pins and list rows look like one product.
 */

export type MapIconId =
  | 'water'
  | 'hut'
  | 'camp'
  | 'fuel'
  | 'repair'
  | 'viewpoint'
  | 'shield'
  | 'surface'
  | 'access'
  | 'search'
  | 'close'
  | 'chevron'
  | 'route'
  | 'gauge'

/** Path data on a 24×24 grid, drawn with a 1.8 round stroke and no fill. */
const PATHS: Record<MapIconId, string> = {
  water: 'M12 3.5c3.2 4 5.5 7.1 5.5 10a5.5 5.5 0 0 1-11 0c0-2.9 2.3-6 5.5-10z',
  hut: 'M3.5 12 12 4.5l8.5 7.5M6 10.3V19.5h12v-9.2M10 19.5v-5h4v5',
  camp: 'M12 4.5 3 19.5h18L12 4.5zM12 4.5v15M9 19.5l3-5 3 5',
  fuel: 'M5.5 19.5v-14a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v14M4.5 19.5h10M7.5 9h4M13.5 9.5h2a1.5 1.5 0 0 1 1.5 1.5v4.5a1.5 1.5 0 0 0 3 0V8.5L17.5 6',
  repair: 'M14.7 6.3a4 4 0 0 0-5.4 5.4L4 17l3 3 5.3-5.3a4 4 0 0 0 5.4-5.4l-2.6 2.6-2.4-.6-.6-2.4 2.6-2.6z',
  viewpoint: 'M2.5 12s3.5-6.5 9.5-6.5 9.5 6.5 9.5 6.5-3.5 6.5-9.5 6.5S2.5 12 2.5 12zM12 9.2a2.8 2.8 0 1 0 0 5.6 2.8 2.8 0 0 0 0-5.6z',
  shield: 'M12 3 4.5 6v5.5c0 4.6 3.2 8.4 7.5 9.5 4.3-1.1 7.5-4.9 7.5-9.5V6L12 3zM8.8 12l2.2 2.2 4.2-4.4',
  surface: 'M3 19.5 9 8l3.5 6 2.5-4 6 9.5H3z',
  access: 'M12 3.5a8.5 8.5 0 1 0 0 17 8.5 8.5 0 0 0 0-17zM6 6l12 12',
  search: 'M10.5 4a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13zM15.3 15.3 20 20',
  close: 'M6 6l12 12M18 6 6 18',
  chevron: 'M9 6l6 6-6 6',
  route: 'M6 18.5a2 2 0 1 0 0-4 2 2 0 0 0 0 4zM18 9.5a2 2 0 1 0 0-4 2 2 0 0 0 0 4zM8 16.5h7.5a2.5 2.5 0 0 0 0-5h-7a2.5 2.5 0 0 1 0-5H16',
  gauge: 'M4.5 16.5a8 8 0 1 1 15 0M12 13.5l4-5M12 14.6a1.1 1.1 0 1 0 0-2.2 1.1 1.1 0 0 0 0 2.2z',
}

export function MapIcon({
  id,
  size = 16,
  className,
  title,
}: {
  id: MapIconId
  size?: number
  className?: string
  title?: string
}) {
  return (
    <svg
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden={title ? undefined : true}
      role={title ? 'img' : undefined}
    >
      {title && <title>{title}</title>}
      <path d={PATHS[id]} />
    </svg>
  )
}

/** The same glyph as markup, for Leaflet divIcons that take an HTML string. */
export function mapIconMarkup(id: MapIconId, size = 14): string {
  return `<svg viewBox="0 0 24 24" width="${size}" height="${size}" fill="none" stroke="currentColor" ` +
    `stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="${PATHS[id]}"/></svg>`
}
