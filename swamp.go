package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

func die(msg string, err error) {
	fmt.Fprintf(os.Stderr, msg+": %v\n", err)
	os.Exit(1)
}

func getCallerId(svc *sts.STS) *sts.GetCallerIdentityOutput {
	output, err := svc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		die("Error fetching caller id", err)
	}

	return output
}

func cleanTokenCode(tokenCode string) string {
	return strings.Trim(tokenCode, " \r\n")
}

func fetchTokenCode(tokenSerialNumber string, cmd string) string {
	fmt.Printf("Obtaining mfa token for: %s\n", tokenSerialNumber)
	if output, err := exec.Command("/bin/sh", "-c", cmd).Output(); err != nil {
		die("Error obtaining mfa token", err)
		return ""
	} else {
		return string(output)
	}
}

func askForTokenCode(tokenSerialNumber string) string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Enter mfa token for %s: ", tokenSerialNumber)
	if tokenCode, err := reader.ReadString('\n'); err != nil {
		die("Error reading mfa token", err)
		return ""
	} else {
		return tokenCode
	}
}

func getTokenCode(config *SwampConfig) string {
	var tokenCode string
	if config.mfaExec != "" {
		tokenCode = fetchTokenCode(config.tokenSerialNumber, config.mfaExec)
	} else {
		tokenCode = askForTokenCode(config.tokenSerialNumber)
	}
	return cleanTokenCode(tokenCode)
}

func validateSessionToken(options session.Options) bool {
	sess := session.Must(session.NewSessionWithOptions(options))
	svc := sts.New(sess)
	_, err := svc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	return err == nil
}

func getSessionToken(options session.Options, config *SwampConfig) *sts.Credentials {
	sess := session.Must(session.NewSessionWithOptions(options))
	svc := sts.New(sess)
	tokenCode := getTokenCode(config)
	output, err := svc.GetSessionToken(&sts.GetSessionTokenInput{
		DurationSeconds: &config.intermediateDuration,
		SerialNumber:    &config.tokenSerialNumber,
		TokenCode:       &tokenCode,
	})
	if err != nil {
		die("Error getting session token", err)
	}

	return output.Credentials
}

func getIntermediateSessionOptions(config *SwampConfig) session.Options {
	return newSessionOptions(&config.intermediateProfile, &config.region)
}

func getBaseSessionOptions(config *SwampConfig) session.Options {
	return newSessionOptions(&config.profile, &config.region)
}

func newSessionOptions(profile, region *string) session.Options {
	return session.Options{
		Config:  aws.Config{Region: region},
		Profile: *profile}
}

// validate session token and request a new one if it's invalid.
// write target profile into .aws/credentials
func ensureSessionTokenProfile(config *SwampConfig, pw *ProfileWriter) {
	if validateSessionToken(getIntermediateSessionOptions(config)) {
		fmt.Printf("Session token for profile %s is still valid\n", config.profile)
	} else {
		cred := getSessionToken(getBaseSessionOptions(config), config)
		if err := pw.WriteProfile(cred, &config.intermediateProfile, &config.region); err != nil {
			die("Error writing profile", err)
		}
	}
}

func assumeRole(svc *sts.STS, roleArn, roleSessionName *string, duration *int64) *sts.Credentials {
	output, err := svc.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         roleArn,
		RoleSessionName: roleSessionName,
		DurationSeconds: duration,
	})
	if err != nil {
		die("Error assuming role", err)
	}

	return output.Credentials
}

// assume-role into target account and write target profile into .aws/credentials
func ensureTargetProfile(config *SwampConfig, pw *ProfileWriter, sess *session.Session) {
	svc := sts.New(sess)

	userId := getCallerId(svc).Arn
	parts := strings.Split(*userId, "/")
	roleSessionName := parts[len(parts)-1]

	cred := assumeRole(svc, config.GetRoleArn(), &roleSessionName, &config.targetDuration)
	if err := pw.WriteProfile(cred, &config.targetProfile, sess.Config.Region); err != nil {
		die("Error writing profile", err)
	}
}

func writeProfileToFile(config *SwampConfig) {
	file, err := os.Create(config.exportFile)
	if err != nil {
		die("Error writing target profile to export file", err)
	}
	defer file.Close()

	fmt.Fprintf(file, "export AWS_PROFILE=%s\nunset AWS_ACCESS_KEY_ID\nunset AWS_SECRET_ACCESS_KEY\n", config.targetProfile)
}

func main() {
	// set up command line flags
	config := NewSwampConfig()
	config.SetupFlags()
	flag.Parse()

	// check user input on command line flags
	if err := config.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(1)
	}
	baseProfile := &config.profile

	if config.tokenSerialNumber != "" {
		baseProfile = &config.intermediateProfile
	}

	pw, err := NewProfileWriter()
	if err != nil {
		die("Error initializing profile writer", err)
	}
	for {
		if config.tokenSerialNumber != "" {
			// get intermediate session token with mfa, use that to assume role into target account
			ensureSessionTokenProfile(config, pw)
		}

		var sess *session.Session
		sess = session.Must(session.NewSessionWithOptions(newSessionOptions(baseProfile, &config.region)))

		ensureTargetProfile(config, pw, sess)

		if config.exportProfile {
			writeProfileToFile(config)
		}

		if !config.renew {
			break
		}
		time.Sleep(time.Second * time.Duration(config.targetDuration/2))
	}
}
