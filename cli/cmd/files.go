// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagFilesConvID string
	flagFilesCat    bool
)

const (
	filesJSONCatLimit = 64 << 20
	filesPullLimit    = 64 << 20
	filesUploadLimit  = 25 << 20
)

var filesCmd = &cobra.Command{
	Use:   "files",
	Short: "Move files between your machine and the local box",
	Long: `Push, pull, list, and read files in the machine's /home/vibecraft tree.

All remote paths are jailed under /home/vibecraft. Symlinks that escape the
jail are rejected.

  vibecraft files ls /home/vibecraft/inbox/
  vibecraft files push ./report.pdf /home/vibecraft/inbox/
  vibecraft files pull /home/vibecraft/systems/x/out.csv ./
  vibecraft files cat /home/vibecraft/COMPUTER.md`,
}

var filesLsCmd = &cobra.Command{
	Use:   "ls [remote-path]",
	Short: "List a directory in /home/vibecraft",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runFilesLs,
}

var filesCatCmd = &cobra.Command{
	Use:   "cat <remote-path>",
	Short: "Print a remote file to stdout",
	Args:  cobra.ExactArgs(1),
	RunE:  runFilesCat,
}

var filesPullCmd = &cobra.Command{
	Use:   "pull <remote-path> <local-path>",
	Short: "Download a remote file to a local path",
	Long: `Download a file from the machine to your local disk. <local-path> may
be an existing directory (the remote basename is appended) or a target file
path.`,
	Args: cobra.ExactArgs(2),
	RunE: runFilesPull,
}

var filesPushCmd = &cobra.Command{
	Use:   "push <local-path | - > <remote-dir-or-path>",
	Short: "Upload a file to the machine",
	Long: `Upload a local file to /home/vibecraft on the machine.

  vibecraft files push ./report.pdf /home/vibecraft/inbox/
  cat data.csv | vibecraft files push - /home/vibecraft/inbox/data.csv

Pushes go through the daemon's existing /api/upload endpoint and are scoped
to a conversation. By default the conversation is "agent-cli"; override with
--conversation-id.`,
	Args: cobra.ExactArgs(2),
	RunE: runFilesPush,
}

func init() {
	filesPushCmd.Flags().StringVar(&flagFilesConvID, "conversation-id", "agent-cli", "Conversation namespace for the upload")
	rootCmd.AddCommand(filesCmd)
	filesCmd.AddCommand(filesLsCmd)
	filesCmd.AddCommand(filesCatCmd)
	filesCmd.AddCommand(filesPullCmd)
	filesCmd.AddCommand(filesPushCmd)
}

func runFilesLs(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	path := ""
	if len(args) == 1 {
		path = args[0]
	}
	resp, err := c.ListFiles(path)
	if err != nil {
		return mapDaemonError(err, "listing files")
	}
	out := schema.FilesLsData{
		Path:    resp.Path,
		Entries: make([]schema.FilesEntryData, 0, len(resp.Entries)),
	}
	for _, e := range resp.Entries {
		out.Entries = append(out.Entries, schema.FilesEntryData{
			Name:  e.Name,
			Path:  e.Path,
			Size:  e.Size,
			Type:  e.Type,
			MTime: e.ModTime,
		})
	}
	return output.Emit(out)
}

func runFilesCat(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	body, _, _, err := c.PullFile(args[0])
	if err != nil {
		return mapDaemonError(err, "reading file")
	}
	defer body.Close()
	// In text mode we just stream to stdout (this is the "agent reads a
	// log file" case). In JSON mode we emit a base64 envelope so the
	// agent can decide what to do with binary content.
	if output.CurrentMode() == output.ModeText {
		_, err := io.Copy(os.Stdout, body)
		return err
	}
	b, err := readAllWithLimit(body, filesJSONCatLimit)
	if err != nil {
		return err
	}
	buf := bytes.NewBuffer(b)
	return output.Emit(map[string]any{
		"path":     args[0],
		"size":     buf.Len(),
		"content":  base64.StdEncoding.EncodeToString(buf.Bytes()),
		"encoding": "base64",
	})
}

func runFilesPull(cmd *cobra.Command, args []string) error {
	remote := args[0]
	local := args[1]

	c, err := newClient()
	if err != nil {
		return err
	}
	body, _, ctype, err := c.PullFile(remote)
	if err != nil {
		return mapDaemonError(err, "downloading file")
	}
	defer body.Close()

	// Resolve target path. If local is an existing dir, append basename.
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		local = filepath.Join(local, filepath.Base(remote))
	}
	abs, err := filepath.Abs(local)
	if err != nil {
		return schema.Newf(schema.CodeInternal, "resolving local path: %s", err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return schema.Newf(schema.CodeInternal, "creating directory: %s", err.Error())
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return schema.Newf(schema.CodeInternal, "opening %s: %s", abs, err.Error())
	}
	defer f.Close()
	written, err := copyWithLimit(f, body, filesPullLimit)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(abs)
		return err
	}
	return output.Emit(schema.FilesPullData{
		RemotePath: remote,
		LocalPath:  abs,
		Bytes:      written,
		MIME:       ctype,
	})
}

func runFilesPush(cmd *cobra.Command, args []string) error {
	localArg := args[0]
	remote := args[1]

	if err := validateInboxPushRemote(localArg, remote); err != nil {
		return err
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	// Read content + name.
	var (
		content io.Reader
		size    int64
		name    string
	)
	if localArg == "-" {
		// Stdin → use remote basename for the filename. If remote is a
		// directory ("ends with /"), require a basename in the path.
		if strings.HasSuffix(remote, "/") {
			return schema.Newf(schema.CodeValidationError,
				"with stdin push, remote must include a filename, not just a directory")
		}
		b, err := readAllWithLimit(os.Stdin, filesUploadLimit)
		if err != nil {
			return err
		}
		content = bytes.NewReader(b)
		size = int64(len(b))
		name = filepath.Base(remote)
	} else {
		f, err := os.Open(localArg)
		if err != nil {
			return schema.Newf(schema.CodePathNotFound, "opening %s: %s", localArg, err.Error())
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return schema.Newf(schema.CodeInternal, "stat: %s", err.Error())
		}
		if info.IsDir() {
			return schema.Newf(schema.CodeValidationError,
				"directory upload not supported yet").
				WithHint("tar | vibecraft files push - is the workaround")
		}
		if info.Size() > filesUploadLimit {
			return schema.Newf(schema.CodeFileTooLarge,
				"file exceeds %d byte cap (daemon /api/upload limit)", filesUploadLimit)
		}
		content = f
		size = info.Size()
		name = filepath.Base(localArg)
	}

	resp, err := c.UploadFile(flagFilesConvID, name, content)
	if err != nil {
		return mapDaemonError(err, "uploading file")
	}
	// The daemon returns {"attachments":[{name,path,...}]} — the first
	// entry is what we just wrote.
	var remotePath string
	if atts, ok := resp["attachments"].([]any); ok && len(atts) > 0 {
		if first, ok := atts[0].(map[string]any); ok {
			if p, ok := first["path"].(string); ok {
				remotePath = p
			}
		}
	}
	return output.Emit(schema.FilesPushData{
		LocalPath:  localArg,
		RemotePath: remotePath,
		Bytes:      size,
		Original:   name,
	})
}

func readAllWithLimit(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, schema.Newf(schema.CodeInternal, "reading body: %s", err.Error())
	}
	if int64(len(b)) > limit {
		return nil, schema.Newf(schema.CodeFileTooLarge,
			"input exceeds %d byte cap", limit).
			WithHint("use a smaller file or split the transfer")
	}
	return b, nil
}

func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, schema.Newf(schema.CodeInternal, "writing file: %s", err.Error())
	}
	if written > limit {
		return written, schema.Newf(schema.CodeFileTooLarge,
			"download exceeds %d byte cap", limit).
			WithHint("use a smaller file or split the transfer")
	}
	return written, nil
}

func validateInboxPushRemote(localArg, remote string) error {
	const inboxRoot = "/home/vibecraft/inbox"
	normalized := filepath.ToSlash(filepath.Clean(remote))
	if remote == "" || (normalized != inboxRoot && !strings.HasPrefix(normalized, inboxRoot+"/")) {
		return schema.Newf(schema.CodeValidationError,
			"files push currently uploads through the inbox; remote must be under %s/", inboxRoot).
			WithHint("use /home/vibecraft/inbox/ or /home/vibecraft/inbox/<filename>")
	}

	// /api/upload stores files in the selected conversation namespace and
	// derives the destination name from the multipart filename. For local
	// file uploads the CLI cannot rename the file remotely; reject paths
	// that imply a different target name so callers don't get a silent
	// upload to a different location than requested.
	if localArg != "-" && !strings.HasSuffix(remote, "/") {
		localName := filepath.Base(localArg)
		remoteName := filepath.Base(normalized)
		if remoteName != "" && remoteName != "." && remoteName != localName {
			return schema.Newf(schema.CodeValidationError,
				"local file upload cannot rename %q to %q through the inbox upload endpoint", localName, remoteName).
				WithHint("rename the local file first, upload to /home/vibecraft/inbox/, or pipe stdin with '-' to choose a filename")
		}
	}
	return nil
}
