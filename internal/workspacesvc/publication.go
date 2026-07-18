package workspacesvc

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

type publicationRefs struct {
	refs   map[string]supervisor.PublishedServiceRef
	exists bool
	err    error
}

func derivePublishedURL(pubCfg supervisor.PublicationConfig, refs publicationRefs, svc config.Service) (string, string) {
	visibility := svc.PublicationVisibilityOrDefault()
	if visibility == "private" {
		return "", ""
	}
	if refs.err != nil {
		return "", "publication_metadata_invalid"
	}
	if ref, ok := refs.refs[svc.Name]; ok {
		if ref.URL != "" && (ref.Visibility == "" || ref.Visibility == visibility) {
			return ref.URL, "route_active"
		}
	}
	if refs.exists && pubCfg.ProviderOrDefault() == "hosted" {
		return "", "publication_platform_url_missing"
	}
	if pubCfg.ProviderOrDefault() == "" {
		return "", "publication_requires_supervisor"
	}
	if pubCfg.ProviderOrDefault() != "hosted" {
		return "", "publication_provider_unsupported"
	}
	tenantSlug := normalizeRouteLabel(pubCfg.TenantSlug, "")
	if tenantSlug == "" {
		return "", "publication_tenant_slug_missing"
	}
	baseDomain := pubCfg.BaseDomainForVisibility(visibility)
	if baseDomain == "" {
		switch visibility {
		case "public":
			return "", "publication_public_base_domain_missing"
		case "tenant":
			return "", "publication_tenant_base_domain_missing"
		default:
			return "", "publication_domain_missing"
		}
	}
	if visibility == "tenant" && strings.TrimSpace(pubCfg.TenantAuth.PolicyRef) == "" {
		return "", "publication_tenant_auth_policy_missing"
	}
	return "", "publication_platform_url_missing"
}

func normalizeRouteLabel(value, fallback string) string {
	return config.NormalizePublicationLabel(value, fallback)
}
