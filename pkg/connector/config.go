package connector

import (
	_ "embed"

	up "go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	DeviceName    string `yaml:"device_name"`
	CryptoDir     string `yaml:"crypto_dir"`
	MaxFileSize   int64  `yaml:"max_file_size"`
	MembersAtOnce int    `yaml:"members_at_once"`
	// AcceptInvitesOn: homeservers (as signed in to) where the account takes every invitation.
	AcceptInvitesOn []string `yaml:"accept_invites_on"`
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "device_name")
	helper.Copy(up.Str, "crypto_dir")
	helper.Copy(up.Int, "max_file_size")
	helper.Copy(up.Int, "members_at_once")
	helper.Copy(up.List, "accept_invites_on")
}

func (mc *MatrixConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &mc.Config, &up.StructUpgrader{
		SimpleUpgrader: up.SimpleUpgrader(upgradeConfig),
		Base:           ExampleConfig,
	}
}
