package loggingproxy

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Host string `yaml:"host"`
	} `yaml:"server"`
	Logging struct {
		Console   bool   `yaml:"console"`
		ServerURL string `yaml:"server_url"`
		Default   bool   `yaml:"default"`
	} `yaml:"logging"`
	Routes map[string]Route `yaml:"routes"`
}

type Route struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
	Logging     bool   `yaml:"logging"`
}
