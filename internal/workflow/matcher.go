package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
)

func payloadMap(payload any) (map[string]any, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	var mapped map[string]any
	if err := json.Unmarshal(data, &mapped); err != nil {
		return nil, fmt.Errorf("unmarshal payload map: %w", err)
	}
	return mapped, nil
}

func matches(expected, actual any) bool {
	switch expectedValue := expected.(type) {
	case map[string]any:
		actualMap, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, childExpected := range expectedValue {
			childActual, ok := actualMap[key]
			if !ok || !matches(childExpected, childActual) {
				return false
			}
		}
		return true
	case []any:
		actualSlice, ok := actual.([]any)
		if !ok {
			return false
		}
		for _, expectedItem := range expectedValue {
			if !anySliceItemMatches(expectedItem, actualSlice) {
				return false
			}
		}
		return true
	default:
		return scalarMatches(expected, actual)
	}
}

func anySliceItemMatches(expected any, actual []any) bool {
	for _, actualItem := range actual {
		if matches(expected, actualItem) {
			return true
		}
	}
	return false
}

func scalarMatches(expected, actual any) bool {
	if expectedFloat, ok := numberAsFloat(expected); ok {
		actualFloat, ok := numberAsFloat(actual)
		return ok && expectedFloat == actualFloat
	}
	return reflect.DeepEqual(expected, actual)
}

func numberAsFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	default:
		return 0, false
	}
}
