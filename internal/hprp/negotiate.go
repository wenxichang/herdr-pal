package hprp

import (
	"encoding/json"
	"sort"
)

// SelectHighestCommonVersions 为每个 family 选择双方都声明的最高版本。
func SelectHighestCommonVersions(client, server []string) []string {
	clientVersions := versionedNames(client)
	serverSet := make(map[string]struct{}, len(server))
	for _, name := range server {
		if _, _, ok := splitVersionedName(name); ok {
			serverSet[name] = struct{}{}
		}
	}
	selected := make(map[string]versionedName)
	for name, candidate := range clientVersions {
		if _, ok := serverSet[name]; !ok {
			continue
		}
		current, exists := selected[candidate.family]
		if !exists || candidate.version > current.version {
			selected[candidate.family] = candidate
		}
	}
	result := make([]string, 0, len(selected))
	for _, candidate := range selected {
		result = append(result, candidate.name)
	}
	sort.Strings(result)
	return result
}

// NegotiateFeatures 选择最高共同 Feature 版本，并返回 Server 提供的连接有效参数。
func NegotiateFeatures(client, server map[string]FeatureOffer) map[string]FeatureOffer {
	clientNames := mapKeys(client)
	serverNames := mapKeys(server)
	selectedNames := SelectHighestCommonVersions(clientNames, serverNames)
	selected := make(map[string]FeatureOffer, len(selectedNames))
	for _, name := range selectedNames {
		selected[name] = cloneFeatureOffer(server[name])
	}
	return selected
}

type versionedName struct {
	name    string
	family  string
	version int
}

func versionedNames(names []string) map[string]versionedName {
	result := make(map[string]versionedName, len(names))
	for _, name := range names {
		family, version, ok := splitVersionedName(name)
		if ok {
			result[name] = versionedName{name: name, family: family, version: version}
		}
	}
	return result
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func cloneFeatureOffer(offer FeatureOffer) FeatureOffer {
	parameters := make(map[string]json.RawMessage, len(offer.Parameters))
	for name, value := range offer.Parameters {
		parameters[name] = append(json.RawMessage(nil), value...)
	}
	return FeatureOffer{Parameters: parameters}
}
