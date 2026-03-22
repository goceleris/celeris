//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	awsRegion      = "us-east-1"
	awsX86Instance = "c6i.xlarge"  // 4 vCPU, 8GB, Intel
	awsArmInstance = "c7g.xlarge"  // 4 vCPU, 8GB, Graviton3
	awsKeyPrefix   = "celeris-mage"
	awsSGName      = "celeris-mage-sg"
)

// cloudArch returns the target architectures from CLOUD_ARCH env (default: "both").
func cloudArch() []string {
	v := os.Getenv("CLOUD_ARCH")
	switch v {
	case "amd64", "x86", "x86_64":
		return []string{"amd64"}
	case "arm64", "aarch64":
		return []string{"arm64"}
	case "", "both":
		return []string{"amd64", "arm64"}
	default:
		return []string{v}
	}
}

// awsEnsureCLI checks that aws CLI is installed and credentials are configured.
func awsEnsureCLI() error {
	if _, err := exec.LookPath("aws"); err != nil {
		return fmt.Errorf("aws CLI not found in PATH; install from https://aws.amazon.com/cli/")
	}
	_, err := awsCLI("sts", "get-caller-identity")
	if err != nil {
		return fmt.Errorf("AWS credentials not configured: %w", err)
	}
	return nil
}

// awsLatestAMI resolves the latest Ubuntu 24.04 AMI for the given arch.
func awsLatestAMI(arch string) (string, error) {
	awsArch := arch
	if arch == "amd64" {
		awsArch = "amd64"
	}
	nameFilter := fmt.Sprintf("ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-%s-server-*", awsArch)
	out, err := awsCLI("ec2", "describe-images",
		"--region", awsRegion,
		"--owners", "099720109477",
		"--filters", fmt.Sprintf("Name=name,Values=%s", nameFilter), "Name=state,Values=available",
		"--query", "sort_by(Images, &CreationDate)[-1].ImageId",
		"--output", "text")
	if err != nil {
		return "", fmt.Errorf("resolve AMI for %s: %w", arch, err)
	}
	ami := strings.TrimSpace(out)
	if ami == "" || ami == "None" {
		return "", fmt.Errorf("no AMI found for %s", arch)
	}
	return ami, nil
}

// awsCreateKeyPair creates an SSH key pair and saves the private key.
func awsCreateKeyPair(dir string) (keyName, keyPath string, err error) {
	keyName = fmt.Sprintf("%s-%d", awsKeyPrefix, time.Now().Unix())
	keyPath = filepath.Join(dir, keyName+".pem")

	out, err := awsCLI("ec2", "create-key-pair",
		"--region", awsRegion,
		"--key-name", keyName,
		"--key-type", "ed25519",
		"--query", "KeyMaterial",
		"--output", "text")
	if err != nil {
		return "", "", fmt.Errorf("create key pair: %w", err)
	}
	// PEM format requires a trailing newline after -----END ... -----.
	pemData := strings.TrimSpace(out) + "\n"
	if err := os.WriteFile(keyPath, []byte(pemData), 0o600); err != nil {
		return "", "", err
	}
	fmt.Printf("  Created key pair: %s\n", keyName)
	return keyName, keyPath, nil
}

// awsCreateSecurityGroup creates an SG allowing SSH inbound.
func awsCreateSecurityGroup() (string, error) {
	out, err := awsCLI("ec2", "create-security-group",
		"--region", awsRegion,
		"--group-name", fmt.Sprintf("%s-%d", awsSGName, time.Now().Unix()),
		"--description", "Celeris mage benchmark/profile VMs",
		"--query", "GroupId",
		"--output", "text")
	if err != nil {
		return "", fmt.Errorf("create security group: %w", err)
	}
	sgID := strings.TrimSpace(out)

	_, err = awsCLI("ec2", "authorize-security-group-ingress",
		"--region", awsRegion,
		"--group-id", sgID,
		"--protocol", "tcp",
		"--port", "22",
		"--cidr", "0.0.0.0/0")
	if err != nil {
		return "", fmt.Errorf("authorize SSH ingress: %w", err)
	}
	fmt.Printf("  Created security group: %s\n", sgID)
	return sgID, nil
}

// awsLaunchInstance launches an EC2 instance and waits for it to be running.
func awsLaunchInstance(ami, instanceType, keyName, sgID, arch string) (instanceID, publicIP string, err error) {
	fmt.Printf("  Launching %s instance (%s)...\n", arch, instanceType)

	out, err := awsCLI("ec2", "run-instances",
		"--region", awsRegion,
		"--image-id", ami,
		"--instance-type", instanceType,
		"--key-name", keyName,
		"--security-group-ids", sgID,
		"--block-device-mappings", `[{"DeviceName":"/dev/sda1","Ebs":{"VolumeSize":30,"VolumeType":"gp3"}}]`,
		"--tag-specifications", fmt.Sprintf(`ResourceType=instance,Tags=[{Key=Name,Value=celeris-mage-%s}]`, arch),
		"--query", "Instances[0].InstanceId",
		"--output", "text")
	if err != nil {
		return "", "", fmt.Errorf("run-instances: %w", err)
	}
	instanceID = strings.TrimSpace(out)

	fmt.Printf("  Waiting for instance %s to be running...\n", instanceID)
	if _, err := awsCLI("ec2", "wait", "instance-running",
		"--region", awsRegion,
		"--instance-ids", instanceID); err != nil {
		return instanceID, "", fmt.Errorf("wait instance-running: %w", err)
	}

	out, err = awsCLI("ec2", "describe-instances",
		"--region", awsRegion,
		"--instance-ids", instanceID,
		"--query", "Reservations[0].Instances[0].PublicIpAddress",
		"--output", "text")
	if err != nil {
		return instanceID, "", fmt.Errorf("get public IP: %w", err)
	}
	publicIP = strings.TrimSpace(out)
	fmt.Printf("  Instance %s running at %s\n", instanceID, publicIP)
	return instanceID, publicIP, nil
}

// awsWaitSSH polls until SSH authentication works on the instance.
// TCP connectivity alone is insufficient — cloud-init may still be setting
// up authorized_keys or restarting sshd.
func awsWaitSSH(ip, keyPath string) error {
	fmt.Printf("  Waiting for SSH on %s...\n", ip)
	// First wait for TCP port to be open.
	for range 60 {
		conn, err := net.DialTimeout("tcp", ip+":22", 2*time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(2 * time.Second)
	}
	// Then verify actual SSH auth works (cloud-init may still be running).
	for attempt := range 30 {
		out, err := awsSSH(ip, keyPath, "echo ready")
		if err == nil && strings.Contains(out, "ready") {
			fmt.Printf("  SSH ready on %s (attempt %d)\n", ip, attempt+1)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("SSH auth timeout on %s after 90s", ip)
}

// awsSCP uploads a local file to the instance.
func awsSCP(src, dst, ip, keyPath string) error {
	return run("scp", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=10",
		"-i", keyPath, src, fmt.Sprintf("ubuntu@%s:%s", ip, dst))
}

// awsSSH runs a command on the instance. The command is passed as the SSH
// remote command, which is interpreted by the remote shell.
func awsSSH(ip, keyPath, cmd string) (string, error) {
	c := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-i", keyPath,
		fmt.Sprintf("ubuntu@%s", ip),
		cmd)
	out, err := c.CombinedOutput()
	return string(out), err
}

// awsSSHStream runs a command on the instance with stdout/stderr streaming.
func awsSSHStream(ip, keyPath, cmd string) error {
	c := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-i", keyPath,
		fmt.Sprintf("ubuntu@%s", ip),
		cmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// awsSCPDown downloads a file from the instance.
func awsSCPDown(remotePath, localPath, ip, keyPath string) error {
	return run("scp", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=10",
		"-i", keyPath, fmt.Sprintf("ubuntu@%s:%s", ip, remotePath), localPath)
}

// awsTerminate terminates an EC2 instance.
func awsTerminate(instanceID string) {
	fmt.Printf("  Terminating instance %s...\n", instanceID)
	_, _ = awsCLI("ec2", "terminate-instances",
		"--region", awsRegion,
		"--instance-ids", instanceID)
}

// awsDeleteKeyPair deletes an SSH key pair.
func awsDeleteKeyPair(keyName string) {
	_, _ = awsCLI("ec2", "delete-key-pair",
		"--region", awsRegion,
		"--key-name", keyName)
}

// awsDeleteSecurityGroup waits for instances to terminate then deletes the SG.
func awsDeleteSecurityGroup(sgID string, instanceIDs ...string) {
	if len(instanceIDs) > 0 {
		args := append([]string{"ec2", "wait", "instance-terminated", "--region", awsRegion, "--instance-ids"}, instanceIDs...)
		_, _ = awsCLI(args...)
	}
	_, _ = awsCLI("ec2", "delete-security-group",
		"--region", awsRegion,
		"--group-id", sgID)
}

// awsCleanup performs best-effort cleanup of all AWS resources.
func awsCleanup(instanceIDs []string, keyName, sgID, keyPath string) {
	fmt.Println("\nCleaning up AWS resources...")
	for _, id := range instanceIDs {
		awsTerminate(id)
	}
	awsDeleteKeyPair(keyName)
	if sgID != "" {
		awsDeleteSecurityGroup(sgID, instanceIDs...)
	}
	if keyPath != "" {
		_ = os.Remove(keyPath)
	}
}

// awsInstanceType returns the appropriate instance type for the architecture.
func awsInstanceType(arch string) string {
	if arch == "arm64" {
		return awsArmInstance
	}
	return awsX86Instance
}

// awsInstallGo installs Go on the instance.
func awsInstallGo(ip, keyPath string) error {
	goVer, err := goVersion()
	if err != nil {
		return err
	}
	arch := "amd64"
	// Detect instance arch from uname.
	uname, _ := awsSSH(ip, keyPath, "uname -m")
	if strings.Contains(uname, "aarch64") {
		arch = "arm64"
	}

	fmt.Printf("  Installing Go %s (%s) on %s...\n", goVer, arch, ip)
	script := installGoScript(goVer, arch)
	// Retry up to 3 times — cloud-init occasionally restarts sshd mid-install.
	for attempt := range 3 {
		out, installErr := awsSSH(ip, keyPath, script)
		if installErr == nil {
			return nil
		}
		fmt.Printf("  Go install attempt %d failed: %v\n    output: %s\n", attempt+1, installErr, strings.TrimSpace(out))
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("Go install failed after 3 attempts on %s", ip)
}

// awsCLI runs an AWS CLI command and returns trimmed stdout.
// Uses separate stdout/stderr to prevent stderr from contaminating output
// (critical for commands like create-key-pair that return sensitive data).
func awsCLI(args ...string) (string, error) {
	cmd := exec.Command("aws", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("aws %s: %w\n%s", strings.Join(args[:min(3, len(args))], " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// jsonPretty formats a value as indented JSON for reports.
func jsonPretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
