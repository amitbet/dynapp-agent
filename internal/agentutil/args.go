package agentutil

import "fmt"

func ObjectArg(args []any, index int) map[string]any {
	if index >= 0 && index < len(args) {
		value, _ := args[index].(map[string]any)
		return value
	}
	return map[string]any{}
}

func SliceArg(args []any, index int) []any {
	if index >= 0 && index < len(args) {
		value, _ := args[index].([]any)
		return value
	}
	return nil
}

func NumberArg(args []any, index, fallback, maximum int) int {
	if len(args) <= index {
		return fallback
	}
	value, ok := args[index].(float64)
	if !ok || value < 0 {
		return fallback
	}
	if value > float64(maximum) {
		return maximum
	}
	return int(value)
}

func StringArg(args []any, index int) string {
	if len(args) <= index {
		return ""
	}
	value, _ := args[index].(string)
	return value
}

func BoolArg(args []any, index int) bool {
	if len(args) <= index {
		return false
	}
	value, _ := args[index].(bool)
	return value
}

func StringValue(value any) string {
	if value == nil {
		return ""
	}
	text, ok := value.(string)
	if ok {
		return text
	}
	return fmt.Sprint(value)
}

func ObjectValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}
