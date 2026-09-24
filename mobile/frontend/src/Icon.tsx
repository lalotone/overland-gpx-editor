const paths = {
  info: 'M12 11v6m0-10v1M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0',
  explore: 'm3 9 6-6 6 6 6-6v15l-6 3-6-6-6 3zM9 3v12m6-6v12',
  plan: 'M5 5h14v14H5zM8 16l3-5 3 2 3-5',
  library: 'M4 4h16v16H4zM8 4v16M12 9h5m-5 4h5',
  offline: 'M12 3v12m-5-5 5 5 5-5M4 16v5h16v-5',
  search: 'M21 21l-6-6M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0',
  layers: 'm3 8 9-5 9 5-9 5zM3 12l9 5 9-5M3 16l9 5 9-5',
  locate: 'M12 2v4m0 12v4M2 12h4m12 0h4M18 12a6 6 0 1 1-12 0 6 6 0 0 1 12 0',
  plus: 'M12 5v14M5 12h14',
  close: 'm6 6 12 12M6 18 18 6',
  undo: 'M9 4 4 9l5 5M4 9h10a6 6 0 0 1 0 12',
  share: 'M12 16V3m-5 5 5-5 5 5M4 14v7h16v-7',
  save: 'M4 3h13l4 4v14H3V3zM7 3v6h10V3M7 21v-8h10v8',
  edit: 'm4 16 12-12 4 4L8 20H4zM14 6l4 4',
  check: 'm4 12 5 5L20 6',
  arrow: 'm9 5 7 7-7 7',
  fit: 'M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5',
  mountain: 'm2 20 7-15 5 10 3-6 5 11z',
  reverse: 'M3 7h17m-4-4 4 4-4 4M21 17H4m4-4-4 4 4 4',
  scissors:
    'm9 9 11 11M9 15 20 4M10 7a3 3 0 1 1-6 0 3 3 0 0 1 6 0M10 17a3 3 0 1 1-6 0 3 3 0 0 1 6 0',
  trash: 'M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7',
} as const
export type IconName = keyof typeof paths
export default function Icon({ name, size = 22 }: { name: IconName; size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={paths[name]} />
    </svg>
  )
}
