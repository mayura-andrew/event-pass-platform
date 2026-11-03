package service

import "strings"

func ValidDisplayName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\r\n")
}
