package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PublicationStoreVersion is the current schema version of the publication store file.
const PublicationStoreVersion = 1

// PublicationStore is the machine-managed authoritative mapping from workspace
// services to externally routable published URLs.
type PublicationStore struct {
	Version int                           `json:"version"`
	Cities  map[string]PublicationCityRef `json:"cities,omitempty"`
}

// PublicationCityRef holds the published service references for a single city.
type PublicationCityRef struct {
	Services []PublishedServiceRef `json:"services,omitempty"`
}

// PublishedServiceRef describes a single published service within a city.
type PublishedServiceRef struct {
	ServiceName string `json:"service_name"`
	Visibility  string `json:"visibility,omitempty"`
	URL         string `json:"url,omitempty"`
}

// LoadCityPublicationRefs reads the publication store at path and returns the service
// references for the given cityPath. The bool indicates whether the store file was found.
func LoadCityPublicationRefs(path, cityPath string) (map[string]PublishedServiceRef, bool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}

	var store PublicationStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, true, fmt.Errorf("decode publication store: %w", err)
	}
	if store.Version != PublicationStoreVersion {
		return nil, true, fmt.Errorf("unsupported publication store version %d", store.Version)
	}

	cityKey := filepath.Clean(cityPath)
	var city PublicationCityRef
	found := false
	for rawKey, candidate := range store.Cities {
		if filepath.Clean(rawKey) == cityKey {
			city = candidate
			found = true
			break
		}
	}
	if !found {
		return map[string]PublishedServiceRef{}, true, nil
	}

	refs := make(map[string]PublishedServiceRef, len(city.Services))
	for _, item := range city.Services {
		name := strings.TrimSpace(item.ServiceName)
		if name == "" {
			continue
		}
		item.ServiceName = name
		item.Visibility = strings.TrimSpace(strings.ToLower(item.Visibility))
		item.URL = strings.TrimSpace(item.URL)
		refs[name] = item
	}
	return refs, true, nil
}
