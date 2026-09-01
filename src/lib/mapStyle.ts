import type { StyleSpecification } from 'maplibre-gl'

type UnknownRecord = Record<string, unknown>

function record(value: unknown): UnknownRecord | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as UnknownRecord
    : null
}

function resolveResource(value: unknown, styleUrl: string): unknown {
  if (typeof value !== 'string' || !value) return value
  try {
    const placeholders: string[] = []
    const protectedValue = value.replace(/\{[^{}]+\}/g, placeholder => {
      placeholders.push(placeholder)
      return `__GPX_MAP_TOKEN_${placeholders.length - 1}__`
    })
    let resolved = new URL(protectedValue, styleUrl).toString()
    placeholders.forEach((placeholder, index) => {
      resolved = resolved.replace(`__GPX_MAP_TOKEN_${index}__`, placeholder)
    })
    return resolved
  } catch {
    return value
  }
}

/** Resolve relative resources because MapLibre receives a style object, not its URL. */
export function resolveMapStyleResources(style: StyleSpecification, styleUrl: string): StyleSpecification {
  const resolved = JSON.parse(JSON.stringify(style)) as StyleSpecification
  const root = resolved as unknown as UnknownRecord

  if (typeof root.glyphs === 'string') root.glyphs = resolveResource(root.glyphs, styleUrl)
  if (Array.isArray(root.sprite)) {
    root.sprite = root.sprite.map(value => {
      const sprite = record(value)
      return sprite ? { ...sprite, url: resolveResource(sprite.url, styleUrl) } : value
    })
  } else if (typeof root.sprite === 'string') {
    root.sprite = resolveResource(root.sprite, styleUrl)
  }

  const sources = record(root.sources)
  if (sources) {
    for (const [id, value] of Object.entries(sources)) {
      const source = record(value)
      if (!source) continue
      const next = { ...source }
      if (typeof next.url === 'string') next.url = resolveResource(next.url, styleUrl)
      if (typeof next.data === 'string') next.data = resolveResource(next.data, styleUrl)
      if (Array.isArray(next.tiles)) {
        next.tiles = next.tiles.map(tile => resolveResource(tile, styleUrl))
      }
      sources[id] = next
    }
  }

  return resolved
}
