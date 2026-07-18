package workspacesvc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestDerivePublishedURLRequiresAuthoritativeHostedMetadata(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{
		Provider:         "hosted",
		TenantSlug:       "Acme",
		PublicBaseDomain: "apps.example.com",
	}, publicationRefs{}, config.Service{
		Name: "review_intake",
		Publication: config.ServicePublicationConfig{
			Visibility: "public",
		},
	})
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if reason != "publication_platform_url_missing" {
		t.Fatalf("reason = %q, want publication_platform_url_missing", reason)
	}
}

func TestDerivePublishedURLUsesAuthoritativeMetadataWhenAvailable(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{
		Provider:         "hosted",
		TenantSlug:       "Acme",
		PublicBaseDomain: "apps.example.com",
	}, publicationRefs{
		refs: map[string]supervisor.PublishedServiceRef{
			"review_intake": {
				ServiceName: "review_intake",
				Visibility:  "public",
				URL:         "https://review-intake--acme--deadbeef.apps.example.com",
			},
		},
		exists: true,
	}, config.Service{
		Name: "review_intake",
		Publication: config.ServicePublicationConfig{
			Visibility: "public",
		},
	})
	if reason != "route_active" {
		t.Fatalf("reason = %q, want route_active", reason)
	}
	if url != "https://review-intake--acme--deadbeef.apps.example.com" {
		t.Fatalf("url = %q, want authoritative hosted route", url)
	}
}

func TestDerivePublishedURLRequiresSupervisor(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{}, publicationRefs{}, config.Service{
		Name: "review-intake",
		Publication: config.ServicePublicationConfig{
			Visibility: "public",
		},
	})
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if reason != "publication_requires_supervisor" {
		t.Fatalf("reason = %q, want publication_requires_supervisor", reason)
	}
}

func TestDerivePublishedURLRequiresTenantSlug(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{
		Provider:         "hosted",
		PublicBaseDomain: "apps.example.com",
	}, publicationRefs{}, config.Service{
		Name: "review-intake",
		Publication: config.ServicePublicationConfig{
			Visibility: "public",
		},
	})
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if reason != "publication_tenant_slug_missing" {
		t.Fatalf("reason = %q, want publication_tenant_slug_missing", reason)
	}
}

func TestDerivePublishedURLRequiresTenantAuthForTenantVisibility(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{
		Provider:         "hosted",
		TenantSlug:       "acme",
		TenantBaseDomain: "tenant.apps.example.com",
	}, publicationRefs{}, config.Service{
		Name: "review-intake",
		Publication: config.ServicePublicationConfig{
			Visibility: "tenant",
		},
	})
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if reason != "publication_tenant_auth_policy_missing" {
		t.Fatalf("reason = %q, want publication_tenant_auth_policy_missing", reason)
	}
}

func TestDerivePublishedURLRequiresConfiguredBaseDomain(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{
		Provider:         "hosted",
		TenantSlug:       strings.Repeat("tenant", 8),
		PublicBaseDomain: strings.Repeat("example", 20) + ".com",
	}, publicationRefs{}, config.Service{
		Name: strings.Repeat("service", 8),
		Publication: config.ServicePublicationConfig{
			Visibility: "public",
		},
	})
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if reason != "publication_platform_url_missing" {
		t.Fatalf("reason = %q, want publication_platform_url_missing", reason)
	}
}

func TestDerivePublishedURLBlocksHostedFallbackWhenAuthoritativeStoreExists(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{
		Provider:         "hosted",
		TenantSlug:       "Acme",
		PublicBaseDomain: "apps.example.com",
	}, publicationRefs{exists: true}, config.Service{
		Name: "review_intake",
		Publication: config.ServicePublicationConfig{
			Visibility: "public",
		},
	})
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if reason != "publication_platform_url_missing" {
		t.Fatalf("reason = %q, want publication_platform_url_missing", reason)
	}
}

func TestDerivePublishedURLReportsPublicationMetadataInvalid(t *testing.T) {
	url, reason := derivePublishedURL(supervisor.PublicationConfig{
		Provider:         "hosted",
		TenantSlug:       "Acme",
		PublicBaseDomain: "apps.example.com",
	}, publicationRefs{
		exists: true,
		err:    fmt.Errorf("decode publication store: boom"),
	}, config.Service{
		Name: "review_intake",
		Publication: config.ServicePublicationConfig{
			Visibility: "public",
		},
	})
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if reason != "publication_metadata_invalid" {
		t.Fatalf("reason = %q, want publication_metadata_invalid", reason)
	}
}
