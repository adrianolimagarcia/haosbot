package marketplace

type SkillItem struct {
	ID               string `json:"id"`
	SkillID          string `json:"skill_id"`
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	Source           string `json:"source"`
	Provider         string `json:"provider"`
	Installs         int64  `json:"installs"`
	Downloads        int64  `json:"downloads,omitempty"`
	URL              string `json:"url,omitempty"`
	Version          string `json:"version,omitempty"`
	Installed        bool   `json:"installed"`
	InstallSupported bool   `json:"install_supported"`
	Verified         bool   `json:"verified,omitempty"`
}

type TrendingResponse struct {
	Skills           []SkillItem `json:"skills"`
	Provider         string      `json:"provider"`
	InstallSupported bool        `json:"install_supported"`
}

type SearchResponse struct {
	Query            string      `json:"query"`
	Skills           []SkillItem `json:"skills"`
	Provider         string      `json:"provider"`
	InstallSupported bool        `json:"install_supported"`
}

type InstallRequest struct {
	Provider string `json:"provider"`
	SkillID  string `json:"skill_id"`
	Source   string `json:"source,omitempty"`
	Version  string `json:"version,omitempty"`
}

type InstallResponse struct {
	Installed bool   `json:"installed"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Message   string `json:"message,omitempty"`
}
