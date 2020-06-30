package driver

import "log"

const (
	terraformVersion = "0.13.0-beta2"
	releasesURL      = "https://releases.hashicorp.com"
)

var _ Driver = (*Terraform)(nil)

// Terraform is an NIA driver that uses the Terraform CLI to interface with
// low-level network infrastructure.
type Terraform struct {
	logLevel   string
	path       string
	dataDir    string
	workingDir string
	skipVerify bool
	backend    map[string]interface{}

	version string
}

// TerraformConfig configures the Terraform driver
type TerraformConfig struct {
	LogLevel   string
	Path       string
	DataDir    string
	WorkingDir string
	SkipVerify bool
	Backend    map[string]interface{}
}

// NewTerraform configures and initializes a new Terraform driver
func NewTerraform(config *TerraformConfig) *Terraform {
	return &Terraform{
		logLevel:   config.LogLevel,
		path:       config.Path,
		dataDir:    config.DataDir,
		workingDir: config.WorkingDir,
		skipVerify: config.SkipVerify,
		backend:    config.Backend,

		version: terraformVersion,
	}
}

// Init initializes the Terraform local environment. The Terraform binary is
// installed to the configured path.
func (tf *Terraform) Init() error {
	if !terraformInstalled(tf.path) {
		log.Printf("[INFO] (driver.terraform) installing terraform (%s) to path %s", tf.version, tf.path)
		if err := tf.install(); err != nil {
			log.Printf("[ERR] (driver.terraform) error installing terraform: %s", err)
			return err
		}
		log.Printf("[INFO] (driver.terraform) successfully installed terraform")
	} else {
		log.Printf("[INFO] (driver.terraform) skipping install, terraform "+
			"already exists at path %s/terraform", tf.path)
	}

	return nil
}

// Version returns the Terraform CLI version for the Terraform driver.
func (tf *Terraform) Version() string {
	return tf.version
}