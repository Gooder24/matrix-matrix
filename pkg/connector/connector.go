package connector

import (
	"context"
	"os"
	"sync"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// MatrixConnector bridges accounts on other Matrix homeservers: each login is a client of its own there,
// with its own device and encryption keys.
type MatrixConnector struct {
	Bridge *bridgev2.Bridge
	Config Config

	signIns sync.Map // login ID → *sync.Mutex: one sign-in of an account finishes at a time
}

var _ bridgev2.NetworkConnector = (*MatrixConnector)(nil)

func (mc *MatrixConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Matrix",
		NetworkURL:           "https://matrix.org",
		NetworkID:            "matrix",
		BeeperBridgeType:     "github.com/Gooder24/matrix-matrix",
		DefaultPort:          29343,
		DefaultCommandPrefix: "!matrix",
	}
}

func (mc *MatrixConnector) Init(bridge *bridgev2.Bridge) { mc.Bridge = bridge }

func (mc *MatrixConnector) Start(ctx context.Context) error {
	if mc.Config.DeviceName == "" {
		mc.Config.DeviceName = "Matrix bridge"
	}
	if mc.Config.CryptoDir == "" {
		mc.Config.CryptoDir = "crypto"
	}
	if mc.Config.MaxFileSize <= 0 {
		mc.Config.MaxFileSize = 25 << 20
	}
	if mc.Config.MembersAtOnce <= 0 {
		mc.Config.MembersAtOnce = 20
	}
	if err := os.MkdirAll(mc.Config.CryptoDir, 0o700); err != nil {
		return err
	}
	mc.addProvisioning()
	return nil
}

func (mc *MatrixConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Reaction:  func() any { return &ReactionMetadata{} },
		UserLogin: func() any { return &UserLoginMetadata{} },
	}
}

func (mc *MatrixConnector) GetBridgeInfoVersion() (info, caps int) { return 1, 1 }

func (mc *MatrixConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	login.Client = newClient(mc, login)
	return nil
}
