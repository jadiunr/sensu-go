package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/sensu/sensu-go/backend"
	"github.com/sensu/sensu-go/backend/seeds"
	etcdstorev1 "github.com/sensu/sensu-go/backend/store/etcd"
	"github.com/sensu/sensu-go/backend/store/postgres"
	etcdstorev2 "github.com/sensu/sensu-go/backend/store/v2/etcdstore"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	defaultTimeout = "5s"

	flagIgnoreAlreadyInitialized = "ignore-already-initialized"
	flagInitAdminUsername        = "cluster-admin-username"
	flagInitAdminPassword        = "cluster-admin-password"
	flagInteractive              = "interactive"
	flagTimeout                  = "timeout"
	flagWait                     = "wait"
	flagInitAdminAPIKey          = "cluster-admin-api-key"
)

// SeedFunc represents the signature of a seed function, used
// to seed the backend store
type SeedFunc func(context.Context, *clientv3.Client, *pgxpool.Pool, *backend.Config) (*backend.Backend, error)

type initConfig struct {
	backend.Config
	SeedConfig seeds.Config
	Timeout    time.Duration
}

func (c *initConfig) Validate() error {
	if c.SeedConfig.AdminUsername == "" || c.SeedConfig.AdminPassword == "" {
		return fmt.Errorf("both %s and %s are required to be set (or an API key)", flagInitAdminUsername, flagInitAdminPassword)
	}
	return nil
}

type initOpts struct {
	AdminUsername             string `survey:"cluster-admin-username"`
	AdminPassword             string `survey:"cluster-admin-password"`
	AdminPasswordConfirmation string `survey:"cluster-admin-password-confirmation"`
	AdminAPIKey               string `survey:"cluster-admin-api-key"`
}

func (i *initOpts) administerQuestionnaire() error {
	qs := []*survey.Question{
		{
			Name: "cluster-admin-username",
			Prompt: &survey.Input{
				Message: "Cluster Admin Username:",
			},
			Validate: survey.Required,
		},
		{
			Name: "cluster-admin-password",
			Prompt: &survey.Password{
				Message: "Cluster Admin Password:",
			},
			Validate: survey.Required,
		},
		{
			Name: "cluster-admin-password-confirmation",
			Prompt: &survey.Password{
				Message: "Retype Cluster Admin Password:",
			},
			Validate: survey.Required,
		},
		{
			Name: "cluster-admin-api-key",
			Prompt: &survey.Input{
				Message: "Cluster Admin API Key:",
			},
		},
	}

	return survey.Ask(qs, i)
}

// InitCommand is the 'sensu-backend init' subcommand.
func InitCommand() *cobra.Command {
	var setupErr error
	cmd := &cobra.Command{
		Use:           "init",
		Short:         "initialize a new sensu installation",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = viper.BindPFlags(cmd.Flags())
			if setupErr != nil {
				return setupErr
			}

			devMode := viper.GetBool(flagDevMode)

			cfg := &backend.Config{
				DevMode: devMode,
				Store: backend.StoreConfig{
					PostgresConfigurationStore: postgres.Config{
						DSN: viper.GetString(flagPGConfigStoreDSN),
					},
					PostgresStateStore: postgres.Config{
						DSN: viper.GetString(flagPGStateStoreDSN),
					},
					EtcdConfigurationStore: etcdstorev1.Config{
						UseEmbeddedClient: viper.GetBool(flagDevMode),
					},
				},
			}

			if cfg.Store.ConfigurationStore != "etcd" && anyConfig(cfg.Store.EtcdConfigurationStore) {
				return errors.New("etcd configuration specified, but config-store is not etcd")
			}

			timeout := viper.GetDuration(flagTimeout)
			if timeout < 1*time.Second {
				timeout = timeout * time.Second
			}

			initConfig := initConfig{
				Config: *cfg,
				SeedConfig: seeds.Config{
					AdminUsername: viper.GetString(flagInitAdminUsername),
					AdminPassword: viper.GetString(flagInitAdminPassword),
					AdminAPIKey:   viper.GetString(flagInitAdminAPIKey),
				},
				Timeout: timeout,
			}

			if viper.GetBool(flagInteractive) {
				var opts initOpts
				if err := opts.administerQuestionnaire(); err != nil {
					return err
				}
				if opts.AdminPassword != opts.AdminPasswordConfirmation {
					//lint:ignore ST1005 this error is written to stdout/stderr
					return errors.New("Password confirmation doesn't match the password")
				}
				initConfig.SeedConfig.AdminUsername = opts.AdminUsername
				initConfig.SeedConfig.AdminPassword = opts.AdminPassword
				initConfig.SeedConfig.AdminAPIKey = opts.AdminAPIKey
			}

			if err := initConfig.Validate(); err != nil {
				return err
			}

			err := initializeStore(initConfig)
			if err != nil {
				if errors.Is(err, seeds.ErrAlreadyInitialized) {
					if viper.GetBool(flagIgnoreAlreadyInitialized) {
						return nil
					}
					return err
				}
				logger.Error(err.Error())
			}
			return err
		},
	}

	cmd.Flags().Bool(flagIgnoreAlreadyInitialized, false, "exit 0 if the cluster has already been initialized")
	cmd.Flags().String(flagInitAdminUsername, "", "cluster admin username")
	cmd.Flags().String(flagInitAdminPassword, "", "cluster admin password")
	cmd.Flags().Bool(flagInteractive, false, "interactive mode")
	cmd.Flags().String(flagTimeout, defaultTimeout, "duration to wait before a connection attempt to etcd is considered failed (must be >= 1s)")
	cmd.Flags().Bool(flagWait, false, "continuously retry to establish a connection to etcd until it is successful")
	cmd.Flags().String(flagInitAdminAPIKey, "", "cluster admin API key")
	cmd.Flags().Bool(flagDevMode, viper.GetBool(flagDevMode), "sensu-backend is running in dev mode")

	setupErr = handleConfig(cmd, os.Args[1:], false)

	return cmd
}

func initializeStore(initConfig initConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), initConfig.Timeout)
	defer cancel()

	if initConfig.DevMode {
		return initializeDevModeStore(ctx, initConfig)
	}

	return nil
}

func initializeDevModeStore(ctx context.Context, initConfig initConfig) error {
	client, err := devModeClient(ctx, &initConfig.Config)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	store := etcdstorev2.NewStore(client)
	nsStore := etcdstorev2.NewNamespaceStore(client)

	if err := seeds.SeedCluster(ctx, store, nsStore, initConfig.SeedConfig); err != nil {
		if errors.Is(err, seeds.ErrAlreadyInitialized) {
			return err
		}
		return fmt.Errorf("error seeding cluster, is cluster healthy? %w", err)
	}
	return nil
}
