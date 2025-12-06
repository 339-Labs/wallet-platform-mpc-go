package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config 应用配置
type Config struct {
	Node    NodeConfig    `yaml:"node"`
	P2P     P2PConfig     `yaml:"p2p"`
	API     APIConfig     `yaml:"api"`
	Storage StorageConfig `yaml:"storage"`
	TSS     TSSConfig     `yaml:"tss"`
	Log     LogConfig     `yaml:"log"`
}

// NodeConfig 节点配置
type NodeConfig struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	KeyFile string `yaml:"key_file"`
}

// P2PConfig P2P网络配置
type P2PConfig struct {
	ListenAddr     string   `yaml:"listen_addr"`
	Port           int      `yaml:"port"`
	BootstrapPeers []string `yaml:"bootstrap_peers"`
	MaxPeers       int      `yaml:"max_peers"`
	EnableRelay    bool     `yaml:"enable_relay"`
	PubSubTopic    string   `yaml:"pubsub_topic"`
}

// APIConfig HTTP API配置
type APIConfig struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	EnableCORS bool   `yaml:"enable_cors"`
	EnableAuth bool   `yaml:"enable_auth"`
	AuthToken  string `yaml:"auth_token"`
}

// StorageConfig 存储配置
type StorageConfig struct {
	DataDir     string `yaml:"data_dir"`
	KeyShareDir string `yaml:"keyshare_dir"`
}

// TSSConfig TSS配置
type TSSConfig struct {
	DefaultThreshold    int `yaml:"default_threshold"`
	DefaultTotalParts   int `yaml:"default_total_parts"`
	KeygenTimeout       int `yaml:"keygen_timeout"`         // 秒
	SignTimeout         int `yaml:"sign_timeout"`           // 秒
	SafePrimeGenTimeout int `yaml:"safe_prime_gen_timeout"` // 秒
}

// LogConfig 日志配置
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	Output string `yaml:"output"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		Node: NodeConfig{
			ID:      "node-1",
			Name:    "MPC Node 1",
			KeyFile: "node.key",
		},
		P2P: P2PConfig{
			ListenAddr:     "0.0.0.0",
			Port:           4001,
			BootstrapPeers: []string{},
			MaxPeers:       50,
			EnableRelay:    true,
			PubSubTopic:    "mpc-wallet",
		},
		API: APIConfig{
			Host:       "0.0.0.0",
			Port:       8080,
			EnableCORS: true,
			EnableAuth: false,
		},
		Storage: StorageConfig{
			DataDir:     "./data",
			KeyShareDir: "./keyshares",
		},
		TSS: TSSConfig{
			DefaultThreshold:    2,
			DefaultTotalParts:   3,
			KeygenTimeout:       300,
			SignTimeout:         60,
			SafePrimeGenTimeout: 120,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
			Output: "stdout",
		},
	}
}

// LoadConfig 从文件加载配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	config := DefaultConfig()
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, err
	}

	return config, nil
}

// SaveConfig 保存配置到文件
func SaveConfig(config *Config, path string) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}
