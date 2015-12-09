package atc

type Pipeline struct {
	Name     string       `json:"name"`
	TeamName string       `json:"team_name"`
	URL      string       `json:"url"`
	Paused   bool         `json:"paused"`
	Groups   GroupConfigs `json:"groups,omitempty"`
}
