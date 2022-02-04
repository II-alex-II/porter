package deploy

import (
	"strconv"
	"strings"
)

// SharedOpts are common options for build, create, and deploy agents
type SharedOpts struct {
	ProjectID       uint
	ClusterID       uint
	Namespace       string
	LocalPath       string
	LocalDockerfile string
	OverrideTag     string
	Method          DeployBuildType
	AdditionalEnv   map[string]string
	EnvGroup        string
}

func getEnvGroupNameVersion(group string) (string, uint, error) {
	if !strings.Contains(group, "@") {
		return group, 0, nil
	}

	envGroupSpl := strings.Split(group, "@")
	envGroupName := envGroupSpl[0]
	envGroupVersion := uint64(0)

	envGroupVersion, err := strconv.ParseUint(envGroupSpl[1], 10, 32)

	if err != nil {
		return "", 0, err
	}

	return envGroupName, uint(envGroupVersion), nil
}
