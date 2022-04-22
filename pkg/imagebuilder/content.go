package imagebuilder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"path"
	template "text/template"

	ignutil "github.com/coreos/ignition/v2/config/util"
	igntypes "github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/vincent-petithory/dataurl"

	"github.com/openshift-agent-team/fleeting/data"
	"github.com/openshift-agent-team/fleeting/pkg/manifests"

	"github.com/openshift/assisted-service/models"
	"github.com/sirupsen/logrus"
)

const NMCONNECTIONS_DIR = "/etc/assisted/network"

// ConfigBuilder builds an Ignition config
type ConfigBuilder struct {
	pullSecret          string
	serviceBaseURL      url.URL
	pullSecretToken     string
	apiVip              string
	controlPlaneAgents  int
	workerAgents        int
	staticNetworkConfig []*models.HostStaticNetworkConfig
	manifestPath        string
}

func New() *ConfigBuilder {
	pullSecret := manifests.GetPullSecret()

	n := manifests.NewNMConfig()
	nodeZeroIP := n.GetNodeZeroIP()

	// TODO: needs appropriate value if AUTH_TYPE != none
	pullSecretToken := getEnv("PULL_SECRET_TOKEN", "")

	serviceBaseURL := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(nodeZeroIP, "8090"),
		Path:   "/",
	}

	aci := manifests.GetAgentClusterInstall()
	clusterInstall := &aci

	infraEnv := manifests.GetInfraEnv()

	staticNetworkConfig, err := manifests.ProcessNMStateConfig(infraEnv)
	if err != nil {
		logrus.Errorf("Error processing NMStateConfigs: %w", err)
		os.Exit(1)
	}

	manifestPath := getEnv("MANIFEST_PATH", "manifests/")

	return &ConfigBuilder{
		pullSecret:          pullSecret,
		serviceBaseURL:      serviceBaseURL,
		pullSecretToken:     pullSecretToken,
		apiVip:              clusterInstall.Spec.APIVIP,
		controlPlaneAgents:  clusterInstall.Spec.ProvisionRequirements.ControlPlaneAgents,
		workerAgents:        clusterInstall.Spec.ProvisionRequirements.WorkerAgents,
		staticNetworkConfig: staticNetworkConfig,
		manifestPath:        manifestPath,
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func ignitionFileEmbed(path string, mode int, overwrite bool, data []byte) igntypes.File {
	source := ignutil.StrToPtr(dataurl.EncodeBytes(data))

	return igntypes.File{
		Node: igntypes.Node{Path: path, Overwrite: &overwrite},
		FileEmbedded1: igntypes.FileEmbedded1{
			Contents: igntypes.Resource{Source: source},
			Mode:     &mode,
		},
	}
}

// Ignition builds an ignition file and returns the bytes
func (c ConfigBuilder) Ignition() ([]byte, error) {
	var err error

	config := igntypes.Config{
		Ignition: igntypes.Ignition{
			Version: igntypes.MaxVersion.String(),
		},
		Passwd: igntypes.Passwd{
			Users: []igntypes.PasswdUser{
				{
					Name:              "core",
					SSHAuthorizedKeys: c.getSSHPubKey(),
				},
			},
		},
	}

	files, err := c.getFiles()
	if err != nil {
		return nil, err
	}

	// pull secret not included in data/ignition/files because embed.FS
	// does not list directories with name starting with '.'
	if c.pullSecret != "" {
		pullSecret := ignitionFileEmbed("/root/.docker/config.json", 0420, true, []byte(c.pullSecret))
		files = append(files, pullSecret)
	}

	if len(c.staticNetworkConfig) > 0 {
		// Get the static network configuration from nmstate and generate NetworkManager ignition files
		filesList, err := manifests.GetNMIgnitionFiles(c.staticNetworkConfig)
		if err == nil {
			for i := range filesList {
				nmFilePath := path.Join(NMCONNECTIONS_DIR, filesList[i].FilePath)
				nmStateIgnFile := ignitionFileEmbed(nmFilePath, 0600, true, []byte(filesList[i].FileContents))
				files = append(files, nmStateIgnFile)
			}

			nmStateScriptFilePath := "/usr/local/bin/pre-network-manager-config.sh"
			// A local version of the assisted-service internal script is currently used
			nmStateScript := ignitionFileEmbed(nmStateScriptFilePath, 0755, true, []byte(manifests.PreNetworkConfigScript))
			files = append(files, nmStateScript)
		} else {
			// If manifest files are invalid, terminate to avoid networking problems at boot
			fmt.Println(err.Error())
			os.Exit(1)
		}
	}

	// add manifests to ignition
	manifests, err := c.getManifests(c.manifestPath)
	if err != nil {
		return nil, err
	}
	files = append(files, manifests...)

	config.Storage.Files = files

	config.Systemd.Units, err = c.getUnits()
	if err != nil {
		return nil, err
	}

	return json.Marshal(config)
}

func (c ConfigBuilder) getSSHPubKey() (keys []igntypes.SSHAuthorizedKey) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	pubkey, err := os.ReadFile(path.Join(home, ".ssh", "id_rsa.pub"))
	if err != nil {
		return
	}
	return append(keys, igntypes.SSHAuthorizedKey(pubkey))
}

func (c ConfigBuilder) getFiles() ([]igntypes.File, error) {
	var readDir func(dirPath string, files []igntypes.File) ([]igntypes.File, error)
	files := make([]igntypes.File, 0)

	readDir = func(dirPath string, files []igntypes.File) ([]igntypes.File, error) {
		entries, err := data.IgnitionData.ReadDir(path.Join("ignition/files", dirPath))
		if err != nil {
			return files, fmt.Errorf("failed to open file dir \"%s\": %w", dirPath, err)
		}
		for _, e := range entries {
			fullPath := path.Join(dirPath, e.Name())
			if e.IsDir() {
				files, err = readDir(fullPath, files)
				if err != nil {
					return files, err
				}
			} else {
				contents, err := data.IgnitionData.ReadFile(path.Join("ignition/files", fullPath))
				if err != nil {
					return files, fmt.Errorf("failed to read file %s: %w", fullPath, err)
				}
				templated, err := c.templateString(e.Name(), string(contents))
				if err != nil {
					return files, err
				}

				mode := 0600
				if _, dirName := path.Split(dirPath); dirName == "bin" || dirName == "dispatcher.d" {
					mode = 0555
				}
				file := ignitionFileEmbed(fullPath, mode, true, []byte(templated))
				files = append(files, file)
			}
		}
		return files, nil
	}

	return readDir("/", files)
}

func (c ConfigBuilder) getUnits() ([]igntypes.Unit, error) {
	units := make([]igntypes.Unit, 0)
	basePath := "ignition/systemd/units"
	staticNetworkService := "pre-network-manager-config.service"

	entries, err := data.IgnitionData.ReadDir(basePath)
	if err != nil {
		return units, fmt.Errorf("failed to read systemd units: %w", err)
	}

	for _, e := range entries {
		if len(c.staticNetworkConfig) == 0 && e.Name() == staticNetworkService {
			continue
		}

		contents, err := data.IgnitionData.ReadFile(path.Join(basePath, e.Name()))
		if err != nil {
			return units, fmt.Errorf("failed to read unit %s: %w", e.Name(), err)
		}

		templated, err := c.templateString(e.Name(), string(contents))
		if err != nil {
			return units, err
		}

		unit := igntypes.Unit{
			Name:     e.Name(),
			Enabled:  ignutil.BoolToPtr(true),
			Contents: ignutil.StrToPtr(string(templated)),
		}
		units = append(units, unit)
	}

	return units, nil
}

// Reads manifests from manifestsPath and adds each file to /etc/assisted/manifests
// in the ignition.
func (c ConfigBuilder) getManifests(manifestPath string) ([]igntypes.File, error) {
	files := make([]igntypes.File, 0)
	entries, err := ioutil.ReadDir(manifestPath)
	if err != nil {
		return files, fmt.Errorf("failed to open file dir \"%s\": %w", manifestPath, err)
	}
	for _, e := range entries {
		localPath := path.Join(manifestPath, e.Name())
		ignitionPath := path.Join("/etc/assisted/manifests", e.Name())
		if e.IsDir() {
			// ignore subdirectories
			continue
		} else {
			contents, err := ioutil.ReadFile(path.Join(localPath))
			if err != nil {
				return files, fmt.Errorf("failed to read file %s: %w", localPath, err)
			}
			mode := 0600
			file := ignitionFileEmbed(ignitionPath, mode, true, contents)
			files = append(files, file)
		}
	}
	return files, nil
}

func (c ConfigBuilder) templateString(name string, text string) (string, error) {
	params := map[string]interface{}{
		"ServiceProtocol":     c.serviceBaseURL.Scheme,
		"ServiceBaseURL":      c.serviceBaseURL.String(),
		"PullSecretToken":     c.pullSecretToken,
		"NodeZeroIP":          c.serviceBaseURL.Hostname(),
		"AssistedServiceHost": c.serviceBaseURL.Host,
		"APIVIP":              c.apiVip,
		"ControlPlaneAgents":  c.controlPlaneAgents,
		"WorkerAgents":        c.workerAgents,
	}

	tmpl, err := template.New(name).Parse(string(text))
	if err != nil {
		return "", err
	}
	buf := &bytes.Buffer{}
	if err = tmpl.Execute(buf, params); err != nil {
		return "", err
	}

	return buf.String(), nil
}
