package kernel

import (
	"strings"

	"github.com/jasonsamtago/Gboard-Node/internal/model"
)

// NeedsGeoIP returns true when any panel route rule contains a "geoip:" match entry.
func NeedsGeoIP(routes []model.RouteRule) bool {
	for _, r := range routes {
		for _, m := range r.Match {
			if strings.HasPrefix(m, "geoip:") {
				return true
			}
		}
	}
	return false
}

// NeedsGeoSite returns true when any panel route rule contains a "geosite:" match entry.
func NeedsGeoSite(routes []model.RouteRule) bool {
	for _, r := range routes {
		for _, m := range r.Match {
			if strings.HasPrefix(m, "geosite:") {
				return true
			}
		}
	}
	return false
}

// NeedsGeo reports whether any route source needs a GeoIP / GeoSite database.
// Official cedar2025/Xboard-Node #20: custom routes must count, not just panel Routes.
func NeedsGeo(nc *model.NodeSpec, kernelCustomRoute []map[string]any) (needIP, needSite bool) {
	if nc == nil {
		return NeedsGeoIPMaps(kernelCustomRoute), NeedsGeoSiteMaps(kernelCustomRoute)
	}
	needIP = NeedsGeoIP(nc.Routes) ||
		NeedsGeoIPRules(nc.CustomRouteRules) ||
		NeedsGeoIPMaps(nc.CustomRoutes) ||
		NeedsGeoIPMaps(kernelCustomRoute)
	needSite = NeedsGeoSite(nc.Routes) ||
		NeedsGeoSiteRules(nc.CustomRouteRules) ||
		NeedsGeoSiteMaps(nc.CustomRoutes) ||
		NeedsGeoSiteMaps(kernelCustomRoute)
	return
}

// NeedsGeoIPMaps returns true when raw custom_routes / custom_route maps contain "geoip:".
func NeedsGeoIPMaps(routes []map[string]any) bool {
	return mapsHavePrefix(routes, "geoip:")
}

// NeedsGeoSiteMaps returns true when raw custom_routes / custom_route maps contain "geosite:".
func NeedsGeoSiteMaps(routes []map[string]any) bool {
	return mapsHavePrefix(routes, "geosite:")
}

func mapsHavePrefix(routes []map[string]any, prefix string) bool {
	for _, route := range routes {
		if valueHasPrefix(route, prefix) {
			return true
		}
	}
	return false
}

func valueHasPrefix(v any, prefix string) bool {
	switch x := v.(type) {
	case string:
		return strings.HasPrefix(x, prefix)
	case []string:
		for _, s := range x {
			if strings.HasPrefix(s, prefix) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if valueHasPrefix(item, prefix) {
				return true
			}
		}
	case map[string]any:
		for _, item := range x {
			if valueHasPrefix(item, prefix) {
				return true
			}
		}
	}
	return false
}
