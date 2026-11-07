package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"video-support-agent/internal/evaluation"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "evaluation:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("use run, review-draft or rescore")
	}
	command := arguments[0]
	if command == "run" {
		return runLive(arguments[1:])
	}
	if command != "review-draft" && command != "rescore" {
		return errors.New("unsupported command")
	}

	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	snapshotPath := flags.String("snapshot", "", "frozen score snapshot")
	reviewPath := flags.String("review", "", "manual review document")
	outputPath := flags.String("out", "", "new output file")
	if err := flags.Parse(arguments[1:]); err != nil {
		return errors.New("invalid command flags")
	}
	if flags.NArg() != 0 || *snapshotPath == "" || *outputPath == "" {
		return errors.New("snapshot and out are required; positional arguments are forbidden")
	}
	if command == "review-draft" && *reviewPath != "" {
		return errors.New("review-draft does not accept review")
	}

	var snapshot evaluation.ScoreSnapshot
	snapshotDigest, err := evaluation.ReadStrictJSON(*snapshotPath, &snapshot)
	if err != nil {
		return err
	}

	if command == "review-draft" {
		draft, err := evaluation.NewReviewDraft(snapshot, snapshotDigest)
		if err != nil {
			return err
		}
		digest, err := evaluation.WriteNewJSON(*outputPath, draft)
		if err != nil {
			return err
		}
		fmt.Println("review draft written; sha256=" + digest)
		return nil
	}

	return rescore(snapshot, snapshotDigest, *reviewPath, *outputPath)
}

func rescore(snapshot evaluation.ScoreSnapshot, snapshotDigest, reviewPath, outputPath string) error {
	var reviews []evaluation.AgentReview
	reviewDigest := ""

	if reviewPath != "" {
		var document evaluation.ReviewDocument
		var err error
		reviewDigest, err = evaluation.ReadStrictJSON(reviewPath, &document)
		if err != nil {
			return err
		}
		if document.SchemaVersion != evaluation.SchemaVersion || document.SnapshotFileSHA256 != snapshotDigest || document.Reviews == nil {
			return errors.New("review document does not match this snapshot")
		}
		reviews = document.Reviews
	}

	report, err := evaluation.ScoreSnapshotData(snapshot, reviews)
	if err != nil {
		return err
	}
	binaryDigest, err := scorerBinaryDigest()
	if err != nil {
		return err
	}
	report.SnapshotFileSHA256 = snapshotDigest
	report.ReviewFileSHA256 = reviewDigest
	report.ScorerBinarySHA256 = binaryDigest

	digest, err := evaluation.WriteNewJSON(outputPath, report)
	if err != nil {
		return err
	}
	fmt.Println("report written; sha256=" + digest)
	return nil
}

func scorerBinaryDigest() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot locate scorer binary")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open scorer binary")
	}

	const limit = 512 << 20
	hash := sha256.New()
	count, readErr := io.Copy(hash, io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || count > limit {
		return "", errors.New("cannot hash scorer binary")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
