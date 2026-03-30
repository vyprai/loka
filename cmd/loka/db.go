package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Manage databases (Postgres, MySQL, Redis)",
		Long: `Managed database instances with automated backups and replication.

Examples:
  loka db create postgres --name mydb
  loka db create redis --name cache --replicas 2
  loka db select mydb
  loka db credentials show
  loka db logs
  loka db backup create`,
	}
	cmd.AddCommand(
		newDBCreateCmd(),
		newDBSelectCmd(),
		newDBListCmd(),
		newDBGetCmd(),
		newDBCredentialsCmd(),
		newDBLogsCmd(),
		newDBStopCmd(),
		newDBStartCmd(),
		newDBDestroyCmd(),
		newDBBackupCmd(),
		newDBReplicaCmd(),
		newDBUpgradeCmd(),
		newDBForceStopCmd(),
	)
	return cmd
}

// resolveDBName returns the database name from the positional arg or the active DB.
func resolveDBName(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	store, _ := loadDeployments()
	if store.ActiveDB != "" {
		return store.ActiveDB, nil
	}
	return "", fmt.Errorf("no database specified. Use a name argument or 'loka db select <name>' first")
}

// resolveDBFlag returns the database name from the --db flag or the active DB.
func resolveDBFlag(dbFlag string) (string, error) {
	if dbFlag != "" {
		return dbFlag, nil
	}
	store, _ := loadDeployments()
	if store.ActiveDB != "" {
		return store.ActiveDB, nil
	}
	return "", fmt.Errorf("no database specified. Use --db <name> or 'loka db select <name>' first")
}

func newDBCreateCmd() *cobra.Command {
	var (
		name            string
		version         string
		password        string
		vcpus           int
		memory          int
		replicas        int
		backupEnabled   bool
		backupSchedule  string
		backupRetention int
		wait            bool
	)

	cmd := &cobra.Command{
		Use:   "create <engine>",
		Short: "Create a database instance",
		Long: `Create a managed database instance.

Supported engines: postgres, mysql, redis

Examples:
  loka db create postgres --name mydb
  loka db create mysql --name appdb --version 8.0 --memory 1024
  loka db create redis --name cache --replicas 2
  loka db create postgres --name prod --replicas 1 --backup-schedule "0 0 * * *"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			engine := args[0]
			client := newClient()

			boolPtr := func(b bool) *bool { return &b }

			body := map[string]any{
				"engine":           engine,
				"name":             name,
				"version":          version,
				"password":         password,
				"vcpus":            vcpus,
				"memory_mb":        memory,
				"replicas":         replicas,
				"backup_enabled":   boolPtr(backupEnabled),
				"backup_schedule":  backupSchedule,
				"backup_retention": backupRetention,
			}

			waitQuery := ""
			if wait {
				waitQuery = "?wait=true"
			}

			var resp struct {
				ID            string `json:"ID"`
				Name          string `json:"Name"`
				Status        string `json:"Status"`
				ImageRef      string `json:"ImageRef"`
				Ready         bool   `json:"Ready"`
				StatusMessage string `json:"StatusMessage"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases"+waitQuery, body, &resp); err != nil {
				return err
			}

			if wait && !resp.Ready {
				fmt.Print("Creating database...")
				for i := 0; i < 120; i++ {
					time.Sleep(1 * time.Second)
					var updated struct {
						Status string `json:"Status"`
						Ready  bool   `json:"Ready"`
					}
					if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+resp.ID, nil, &updated); err != nil {
						break
					}
					if updated.Ready || updated.Status == "running" {
						break
					}
					if updated.Status == "error" {
						fmt.Println(" FAILED")
						return fmt.Errorf("database creation failed")
					}
					fmt.Print(".")
				}
				fmt.Println(" ready!")
			}

			fmt.Printf("Database %s (%s) created\n", resp.Name, resp.ImageRef)

			// Auto-select this DB.
			store, _ := loadDeployments()
			store.ActiveDB = resp.Name
			saveDeployments(store)

			// Show credentials.
			var creds struct {
				Host     string `json:"host"`
				Port     int    `json:"port"`
				Username string `json:"username"`
				Password string `json:"password"`
				DBName   string `json:"db_name"`
				URL      string `json:"url"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+resp.ID+"/credentials", nil, &creds); err == nil {
				fmt.Printf("  Host:     %s\n", creds.Host)
				fmt.Printf("  Port:     %d\n", creds.Port)
				if creds.Username != "" {
					fmt.Printf("  User:     %s\n", creds.Username)
				}
				fmt.Printf("  Password: %s\n", creds.Password)
				if creds.DBName != "" {
					fmt.Printf("  Database: %s\n", creds.DBName)
				}
				fmt.Printf("  URL:      %s\n", creds.URL)
			}

			if replicas > 0 {
				fmt.Printf("  Replicas: %d\n", replicas)
			}
			fmt.Printf("\nUse --db %s with 'loka deploy' to connect services.\n", resp.Name)

			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Database name (auto-generated if omitted)")
	cmd.Flags().StringVar(&version, "version", "", "Engine version (default: latest stable)")
	cmd.Flags().StringVar(&password, "password", "", "Password (auto-generated if omitted)")
	cmd.Flags().IntVar(&vcpus, "vcpus", 1, "CPU cores")
	cmd.Flags().IntVar(&memory, "memory", 512, "Memory in MB")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "Number of read replicas")
	cmd.Flags().BoolVar(&backupEnabled, "backup", true, "Enable automated backups")
	cmd.Flags().StringVar(&backupSchedule, "backup-schedule", "0 */6 * * *", "Backup cron schedule")
	cmd.Flags().IntVar(&backupRetention, "backup-retention", 7, "Backup retention in days")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait for database to be ready")
	return cmd
}

func newDBSelectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "select <name>",
		Short: "Set the active database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Verify database exists.
			client := newClient()
			var resp struct {
				Name   string `json:"Name"`
				Status string `json:"Status"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+name, nil, &resp); err != nil {
				return fmt.Errorf("database %q not found", name)
			}

			store, _ := loadDeployments()
			store.ActiveDB = name
			saveDeployments(store)
			fmt.Printf("Active database: %s (%s)\n", resp.Name, resp.Status)
			return nil
		},
	}
}

func newDBListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List databases", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Databases []struct {
					ID             string `json:"ID"`
					Name           string `json:"Name"`
					Status         string `json:"Status"`
					ImageRef       string `json:"ImageRef"`
					Ready          bool   `json:"Ready"`
					DatabaseConfig *struct {
						Engine  string `json:"engine"`
						Version string `json:"version"`
						Role    string `json:"role"`
					} `json:"DatabaseConfig"`
					CreatedAt time.Time `json:"CreatedAt"`
				} `json:"databases"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			if len(resp.Databases) == 0 {
				fmt.Println("No databases. Create one: loka db create postgres --name mydb")
				return nil
			}

			store, _ := loadDeployments()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "  NAME\tENGINE\tROLE\tSTATUS\tCREATED")
			for _, d := range resp.Databases {
				engine := d.ImageRef
				role := ""
				if d.DatabaseConfig != nil {
					engine = d.DatabaseConfig.Engine + ":" + d.DatabaseConfig.Version
					role = d.DatabaseConfig.Role
				}
				status := d.Status
				if d.Ready {
					status += " (ready)"
				}
				active := " "
				if d.Name == store.ActiveDB {
					active = "*"
				}
				fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\t%s\n",
					active, d.Name, engine, role, status, d.CreatedAt.Format("2006-01-02 15:04"))
			}
			w.Flush()
			return nil
		},
	}
}

func newDBGetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "get [name]", Short: "Get database details", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			client := newClient()
			var svc struct {
				ID             string `json:"ID"`
				Name           string `json:"Name"`
				Status         string `json:"Status"`
				ImageRef       string `json:"ImageRef"`
				WorkerID       string `json:"WorkerID"`
				GuestIP        string `json:"GuestIP"`
				VCPUs          int    `json:"VCPUs"`
				MemoryMB       int    `json:"MemoryMB"`
				Ready          bool   `json:"Ready"`
				StatusMessage  string `json:"StatusMessage"`
				DatabaseConfig *struct {
					Engine    string `json:"engine"`
					Version   string `json:"version"`
					Role      string `json:"role"`
					PrimaryID string `json:"primary_id"`
				} `json:"DatabaseConfig"`
				CreatedAt time.Time `json:"CreatedAt"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+name, nil, &svc); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(svc)
			}
			fmt.Printf("Name:     %s\n", svc.Name)
			fmt.Printf("ID:       %s\n", svc.ID)
			if svc.DatabaseConfig != nil {
				fmt.Printf("Engine:   %s:%s\n", svc.DatabaseConfig.Engine, svc.DatabaseConfig.Version)
				fmt.Printf("Role:     %s\n", svc.DatabaseConfig.Role)
				if svc.DatabaseConfig.PrimaryID != "" {
					fmt.Printf("Primary:  %s\n", svc.DatabaseConfig.PrimaryID)
				}
			}
			fmt.Printf("Status:   %s\n", svc.Status)
			if svc.StatusMessage != "" {
				fmt.Printf("Message:  %s\n", svc.StatusMessage)
			}
			fmt.Printf("Ready:    %v\n", svc.Ready)
			fmt.Printf("vCPUs:    %d\n", svc.VCPUs)
			fmt.Printf("Memory:   %d MB\n", svc.MemoryMB)
			fmt.Printf("Worker:   %s\n", svc.WorkerID)
			if svc.GuestIP != "" {
				fmt.Printf("IP:       %s\n", svc.GuestIP)
			}
			fmt.Printf("Created:  %s\n", svc.CreatedAt.Format("2006-01-02 15:04:05"))
			return nil
		},
	}
}

func newDBCredentialsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Manage database credentials",
	}
	cmd.AddCommand(
		newDBCredentialsShowCmd(),
		newDBCredentialsRotateCmd(),
		newDBCredentialsSetCmd(),
	)
	return cmd
}

func newDBCredentialsShowCmd() *cobra.Command {
	var dbName string
	cmd := &cobra.Command{
		Use: "show", Short: "Show database credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			client := newClient()
			var creds struct {
				Engine    string `json:"engine"`
				Version   string `json:"version"`
				Host      string `json:"host"`
				Port      int    `json:"port"`
				LoginRole string `json:"login_role"`
				GroupRole string `json:"group_role"`
				Password  string `json:"password"`
				DBName    string `json:"db_name"`
				URL       string `json:"url"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+name+"/credentials", nil, &creds); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(creds)
			}
			fmt.Printf("Engine:     %s:%s\n", creds.Engine, creds.Version)
			fmt.Printf("Host:       %s\n", creds.Host)
			fmt.Printf("Port:       %d\n", creds.Port)
			if creds.LoginRole != "" {
				fmt.Printf("Login:      %s\n", creds.LoginRole)
			}
			if creds.GroupRole != "" {
				fmt.Printf("Group role: %s\n", creds.GroupRole)
			}
			fmt.Printf("Password:   %s\n", creds.Password)
			if creds.DBName != "" {
				fmt.Printf("Database:   %s\n", creds.DBName)
			}
			fmt.Printf("URL:        %s\n", creds.URL)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name (uses selected DB if omitted)")
	return cmd
}

func newDBCredentialsRotateCmd() *cobra.Command {
	var dbName string
	cmd := &cobra.Command{
		Use: "rotate", Short: "Rotate database password",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			client := newClient()
			var resp struct {
				LoginRole         string `json:"login_role"`
				Password          string `json:"password"`
				URL               string `json:"url"`
				PreviousLoginRole string `json:"previous_login_role"`
				GracePeriod       string `json:"grace_period"`
				GraceDeadline     string `json:"grace_deadline"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/credentials/rotate", nil, &resp); err != nil {
				return err
			}
			fmt.Printf("Credentials rotated\n")
			fmt.Printf("  Login role:  %s\n", resp.LoginRole)
			fmt.Printf("  Password:    %s\n", resp.Password)
			fmt.Printf("  URL:         %s\n", resp.URL)
			fmt.Printf("  Old login:   %s (valid until %s)\n", resp.PreviousLoginRole, resp.GraceDeadline)
			fmt.Printf("  Grace:       %s\n", resp.GracePeriod)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name (uses selected DB if omitted)")
	return cmd
}

func newDBCredentialsSetCmd() *cobra.Command {
	var (
		dbName   string
		password string
	)
	cmd := &cobra.Command{
		Use: "set", Short: "Set database password",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			if password == "" {
				return fmt.Errorf("--password is required")
			}
			client := newClient()
			var resp struct {
				Password string `json:"password"`
				URL      string `json:"url"`
			}
			if err := client.Raw(cmd.Context(), "PUT", "/api/v1/databases/"+name+"/credentials", map[string]any{"password": password}, &resp); err != nil {
				return err
			}
			fmt.Printf("Password updated\n")
			fmt.Printf("  URL: %s\n", resp.URL)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name (uses selected DB if omitted)")
	cmd.Flags().StringVar(&password, "password", "", "New password")
	return cmd
}

func newDBLogsCmd() *cobra.Command {
	var (
		follow bool
		lines  int
	)
	cmd := &cobra.Command{
		Use: "logs [name]", Short: "View database logs", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			client := newClient()

			fetchLogs := func() error {
				var resp struct {
					Stdout []string `json:"stdout"`
					Stderr []string `json:"stderr"`
				}
				path := fmt.Sprintf("/api/v1/databases/%s/logs?lines=%d", name, lines)
				if err := client.Raw(cmd.Context(), "GET", path, nil, &resp); err != nil {
					return err
				}
				for _, line := range resp.Stdout {
					fmt.Println(line)
				}
				for _, line := range resp.Stderr {
					fmt.Fprintf(os.Stderr, "%s\n", line)
				}
				return nil
			}

			if err := fetchLogs(); err != nil {
				return err
			}
			if follow {
				for {
					time.Sleep(2 * time.Second)
					if err := fetchLogs(); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "Number of lines to show")
	return cmd
}

func newDBStopCmd() *cobra.Command {
	return &cobra.Command{
		Use: "stop [name]", Short: "Stop a database", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			client := newClient()
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/stop", nil, nil); err != nil {
				return err
			}
			fmt.Printf("Database %s stopped\n", name)
			return nil
		},
	}
}

func newDBStartCmd() *cobra.Command {
	return &cobra.Command{
		Use: "start [name]", Short: "Start a stopped database", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			client := newClient()
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/start", nil, nil); err != nil {
				return err
			}
			fmt.Printf("Database %s started\n", name)
			return nil
		},
	}
}

func newDBDestroyCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use: "destroy [name]", Short: "Destroy a database and its data", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			if !force {
				fmt.Printf("Destroy database %q and all its data? (yes/no): ", name)
				var confirm string
				fmt.Scanln(&confirm)
				if confirm != "yes" {
					fmt.Println("Aborted.")
					return nil
				}
			}
			client := newClient()
			if err := client.Raw(cmd.Context(), "DELETE", "/api/v1/databases/"+name, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Database %s destroyed\n", name)

			// Clear active DB if it was this one.
			store, _ := loadDeployments()
			if store.ActiveDB == name {
				store.ActiveDB = ""
				saveDeployments(store)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation")
	return cmd
}

func newDBBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Manage database backups",
	}
	cmd.AddCommand(
		newDBBackupCreateCmd(),
		newDBBackupListCmd(),
		newDBBackupRestoreCmd(),
	)
	return cmd
}

func newDBBackupCreateCmd() *cobra.Command {
	var dbName string
	cmd := &cobra.Command{
		Use: "create", Short: "Create a manual backup",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			client := newClient()
			var resp struct {
				BackupID string `json:"backup_id"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/backups", nil, &resp); err != nil {
				return err
			}
			fmt.Printf("Backup created: %s\n", resp.BackupID)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name")
	return cmd
}

func newDBBackupListCmd() *cobra.Command {
	var dbName string
	cmd := &cobra.Command{
		Use: "list", Short: "List backups", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			client := newClient()
			var resp struct {
				Backups []struct {
					ID        string    `json:"id"`
					Type      string    `json:"type"`
					Size      int64     `json:"size"`
					CreatedAt time.Time `json:"created_at"`
				} `json:"backups"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+name+"/backups", nil, &resp); err != nil {
				return err
			}
			if len(resp.Backups) == 0 {
				fmt.Println("No backups yet.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tTYPE\tSIZE\tCREATED")
			for _, b := range resp.Backups {
				fmt.Fprintf(w, "%s\t%s\t%d MB\t%s\n", b.ID, b.Type, b.Size/(1024*1024), b.CreatedAt.Format("2006-01-02 15:04"))
			}
			w.Flush()
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name")
	return cmd
}

func newDBBackupRestoreCmd() *cobra.Command {
	var (
		dbName      string
		backupID    string
		pointInTime string
	)
	cmd := &cobra.Command{
		Use: "restore", Short: "Restore from a backup",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			client := newClient()
			body := map[string]any{}
			if backupID != "" {
				body["backup_id"] = backupID
			}
			if pointInTime != "" {
				body["point_in_time"] = pointInTime
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/restore", body, nil); err != nil {
				return err
			}
			fmt.Printf("Database %s restore initiated\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name")
	cmd.Flags().StringVar(&backupID, "backup-id", "", "Backup ID to restore from")
	cmd.Flags().StringVar(&pointInTime, "point-in-time", "", "Point-in-time timestamp (e.g., 2026-03-29T14:00:00Z)")
	return cmd
}

func newDBReplicaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replica",
		Short: "Manage database replicas",
	}
	cmd.AddCommand(
		newDBReplicaAddCmd(),
		newDBReplicaRemoveCmd(),
		newDBReplicaListCmd(),
	)
	return cmd
}

func newDBReplicaAddCmd() *cobra.Command {
	var (
		dbName string
		count  int
	)
	cmd := &cobra.Command{
		Use: "add", Short: "Add read replica(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			client := newClient()
			var resp struct {
				Replicas []struct {
					Name   string `json:"Name"`
					Status string `json:"Status"`
				} `json:"replicas"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/replicas", map[string]any{"count": count}, &resp); err != nil {
				return err
			}
			for _, r := range resp.Replicas {
				fmt.Printf("Replica %s created (%s)\n", r.Name, r.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name")
	cmd.Flags().IntVar(&count, "count", 1, "Number of replicas to add")
	return cmd
}

func newDBReplicaRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "remove <replica-name>", Short: "Remove a replica", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			replicaName := args[0]
			client := newClient()
			// Find the primary to get the right API path.
			var svc struct {
				ID             string `json:"ID"`
				DatabaseConfig *struct {
					PrimaryID string `json:"primary_id"`
				} `json:"DatabaseConfig"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+replicaName, nil, &svc); err != nil {
				return err
			}
			primaryID := ""
			if svc.DatabaseConfig != nil {
				primaryID = svc.DatabaseConfig.PrimaryID
			}
			if primaryID == "" {
				return fmt.Errorf("%s is not a replica", replicaName)
			}
			if err := client.Raw(cmd.Context(), "DELETE", "/api/v1/databases/"+primaryID+"/replicas/"+svc.ID, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Replica %s removed\n", replicaName)
			return nil
		},
	}
}

func newDBReplicaListCmd() *cobra.Command {
	var dbName string
	cmd := &cobra.Command{
		Use: "list", Short: "List replicas", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBFlag(dbName)
			if err != nil {
				return err
			}
			client := newClient()
			var resp struct {
				Replicas []struct {
					ID             string `json:"ID"`
					Name           string `json:"Name"`
					Status         string `json:"Status"`
					GuestIP        string `json:"GuestIP"`
					DatabaseConfig *struct {
						Role string `json:"role"`
					} `json:"DatabaseConfig"`
				} `json:"replicas"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/databases/"+name+"/replicas", nil, &resp); err != nil {
				return err
			}
			if len(resp.Replicas) == 0 {
				fmt.Println("No replicas.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tROLE\tSTATUS\tIP")
			for _, r := range resp.Replicas {
				role := "replica"
				if r.DatabaseConfig != nil {
					role = r.DatabaseConfig.Role
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, role, r.Status, r.GuestIP)
			}
			w.Flush()
			return nil
		},
	}
	cmd.Flags().StringVar(&dbName, "db", "", "Database name")
	return cmd
}

func newDBForceStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "force-stop [name]",
		Short: "Force-stop a stuck database",
		Long:  "Force-stop a database that is stuck in deploying, upgrading, or other non-responsive state.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			client := newClient()
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/force-stop", nil, nil); err != nil {
				return err
			}
			fmt.Printf("Database %s force-stopped\n", name)
			return nil
		},
	}
}

func newDBUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade or rollback database version",
	}
	cmd.AddCommand(
		newDBUpgradeRunCmd(),
		newDBUpgradeRollbackCmd(),
	)
	return cmd
}

func newDBUpgradeRunCmd() *cobra.Command {
	var targetVersion string
	cmd := &cobra.Command{
		Use:   "run [name]",
		Short: "Upgrade database to a new version",
		Long: `Upgrade a database engine version (e.g., MySQL 5.7 to 8.0).

Creates a backup before upgrading. Use 'loka db upgrade rollback' to revert.

Examples:
  loka db upgrade run --target-version 8.0
  loka db upgrade run mydb --target-version 16`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			if targetVersion == "" {
				return fmt.Errorf("--target-version is required")
			}
			client := newClient()
			var resp struct {
				Status          string `json:"status"`
				PreviousVersion string `json:"previous_version"`
				TargetVersion   string `json:"target_version"`
				BackupID        string `json:"backup_id"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/upgrade",
				map[string]any{"target_version": targetVersion}, &resp); err != nil {
				return err
			}
			fmt.Printf("Upgrading %s: %s -> %s\n", name, resp.PreviousVersion, resp.TargetVersion)
			if resp.BackupID != "" {
				fmt.Printf("  Pre-upgrade backup: %s\n", resp.BackupID)
			}
			fmt.Println("  Rollback: loka db upgrade rollback")
			return nil
		},
	}
	cmd.Flags().StringVar(&targetVersion, "target-version", "", "Target engine version")
	return cmd
}

func newDBUpgradeRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback [name]",
		Short: "Rollback to previous database version",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveDBName(args)
			if err != nil {
				return err
			}
			client := newClient()
			var resp struct {
				Status          string `json:"status"`
				RestoredVersion string `json:"restored_version"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/databases/"+name+"/upgrade/rollback", nil, &resp); err != nil {
				return err
			}
			fmt.Printf("Rolling back %s to version %s\n", name, resp.RestoredVersion)
			return nil
		},
	}
}

