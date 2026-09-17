package env

import "fmt"

type Profile string

const (
	ProfileBasic          Profile = "basic"
	ProfileGVisor         Profile = "gvisor"
	ProfileKataQemu       Profile = "kata-qemu"
	ProfileKataClh        Profile = "kata-clh"
	ProfileKataFc         Profile = "kata-fc"
	ProfileKataDragonball Profile = "kata-dragonball"
)

type RuntimeKind string

const (
	RuntimeContainer      RuntimeKind = "container"
	RuntimeGVisor         RuntimeKind = "gvisor"
	RuntimeKataQemu       RuntimeKind = "kata-qemu"
	RuntimeKataClh        RuntimeKind = "kata-clh"
	RuntimeKataFc         RuntimeKind = "kata-fc"
	RuntimeKataDragonball RuntimeKind = "kata-dragonball"
)

const (
	kindNodeImage      = "kindest/node:v1.31.0"
	kataClusterName    = "fsb-e2e-kata"
	kataKindConfigPath = "test/e2e/manifests/kind/kata.yaml"
)

type ProfileSettings struct {
	ClusterName string
	KindConfig  string
	KindImage   string
	Runtime     RuntimeKind
}

func (p Profile) Settings() (ProfileSettings, error) {
	switch p {
	case ProfileBasic:
		return ProfileSettings{
			ClusterName: "fsb-e2e-basic",
			KindImage:   "kindest/node:v1.27.3",
			Runtime:     RuntimeContainer,
		}, nil
	case ProfileGVisor:
		return ProfileSettings{
			ClusterName: "fsb-e2e-gvisor",
			KindConfig:  "test/e2e/manifests/kind/gvisor.yaml",
			KindImage:   kindNodeImage,
			Runtime:     RuntimeGVisor,
		}, nil
	case ProfileKataQemu:
		return ProfileSettings{
			ClusterName: kataClusterName,
			KindConfig:  kataKindConfigPath,
			KindImage:   kindNodeImage,
			Runtime:     RuntimeKataQemu,
		}, nil
	case ProfileKataClh:
		return ProfileSettings{
			ClusterName: kataClusterName,
			KindConfig:  kataKindConfigPath,
			KindImage:   kindNodeImage,
			Runtime:     RuntimeKataClh,
		}, nil
	case ProfileKataFc:
		return ProfileSettings{
			ClusterName: kataClusterName,
			KindConfig:  kataKindConfigPath,
			KindImage:   kindNodeImage,
			Runtime:     RuntimeKataFc,
		}, nil
	case ProfileKataDragonball:
		return ProfileSettings{
			ClusterName: kataClusterName,
			KindConfig:  kataKindConfigPath,
			KindImage:   kindNodeImage,
			Runtime:     RuntimeKataDragonball,
		}, nil
	default:
		return ProfileSettings{}, fmt.Errorf("unknown e2e profile %q", p)
	}
}
