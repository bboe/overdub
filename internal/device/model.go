package device

import "strings"

var models = map[string]string{
	"biscuit": "Echo Dot (2nd Generation)",
	"donut":   "Echo Dot (3rd Generation)",
	"radar":   "Echo (2nd Generation)",
}

func Board() string {
	board, _, _ := strings.Cut(Getprop("ro.product.device"), "_")
	return board
}

func ModelName(board string) string {
	if name, ok := models[board]; ok {
		return name
	}
	if board == "" {
		return "Echo"
	}
	return "Echo (" + board + ")"
}
