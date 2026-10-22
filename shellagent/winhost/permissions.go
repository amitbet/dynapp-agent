package winhost

type permissionGroup struct {
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Danger      string   `json:"danger"`
	Permissions []string `json:"permissions"`
	Reasons     []string `json:"reasons"`
	Granted     bool     `json:"granted"`
	Suggested   bool     `json:"suggested"`
}

// permissionInitiallyChecked mirrors the macOS permission sheet. Reviews keep
// current grants; new requests require an explicit choice for very-high risk.
func permissionInitiallyChecked(kind string, group permissionGroup) bool {
	if kind == "review" {
		return group.Granted
	}
	return group.Suggested && group.Danger != "very-high"
}

func selectedCapabilities(groups []permissionGroup, checked func(int) bool) []string {
	capabilities := []string{}
	for i, group := range groups {
		if checked(i) {
			capabilities = append(capabilities, group.Permissions...)
		}
	}
	return capabilities
}
