package server

// The default map, routing and terrain sources are the project's own mirror,
// an oms (https://code.rbel.co/rubiojr/oms) instance. It fetches on demand
// from OpenFreeMap, Geofabrik and the AWS Skadi and Terrarium sets and keeps a copy, so
// every overland deployment shares one cache and the public services see one
// request per object rather than one per user. Reads need no token; an oms
// API token only protects its mirror-job endpoints, which overland never
// calls. Override with OPENFREEMAP_URL, ROUTING_*_URL and ELEVATION_TILE_URL
// to use another mirror or the public services directly.
const (
	DefaultMirrorURL               = "https://oms.rbel.co"
	DefaultOpenFreeMapURL          = DefaultMirrorURL + "/maps/styles/liberty"
	DefaultRoutingIndexURL         = DefaultMirrorURL + "/routing/osm/index-v1.json"
	DefaultRoutingMetadataIndexURL = DefaultMirrorURL + "/routing/osm/index-v1-nogeom.json"
	DefaultRoutingPBFBaseURL       = DefaultMirrorURL + "/routing/osm"
	DefaultRoutingDEMBaseURL       = DefaultMirrorURL + "/routing/skadi"
	DefaultElevationTileURL        = DefaultMirrorURL + "/elevation/terrarium/{z}/{x}/{y}.png"
)
