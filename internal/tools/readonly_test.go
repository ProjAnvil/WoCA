package tools

import "testing"

func TestIsReadOnlyCommand(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    bool
	}{
		// Basic read-only commands.
		{"ls", "ls -la", true},
		{"cat", "cat file.go", true},
		{"grep", "grep -r foo .", true},
		{"git status", "git status", true},
		{"git log", "git log --oneline", true},
		{"git diff", "git diff", true},
		{"find", "find . -name '*.go'", true},
		{"echo", "echo hello", true},
		{"pwd", "pwd", true},
		{"input redirect only", "cat < file.txt", true},

		// Basic write commands.
		{"go test", "go test ./...", false},
		{"go build", "go build ./...", false},
		{"rm", "rm -rf /tmp/x", false},
		{"git push", "git push origin main", false},
		{"git commit", "git commit -m 'x'", false},
		{"curl", "curl https://example.com", false},
		{"npm install", "npm install", false},

		// Wrappers.
		{"sudo ls", "sudo ls -la", true},
		{"sudo rm", "sudo rm -rf /tmp/x", false},
		{"command ls", "command ls", true},
		{"command -v", "command -v ls", true},
		{"command rm", "command rm -rf x", false},
		{"env ls", "env ls", true},
		{"env assign ls", "env FOO=bar ls", true},
		{"env assign rm", "env FOO=bar rm -rf x", false},
		{"nice ls", "nice ls", true},
		{"nice rm", "nice rm x", false},
		{"nohup ls", "nohup ls", true},
		{"nohup rm", "nohup rm x", false},
		{"timeout ls", "timeout 5 ls", true},
		{"timeout rm", "timeout 5 rm x", false},
		{"timeout signal ls", "timeout -s KILL 5 ls", true},
		{"timeout signal rm", "timeout -s KILL 5 rm x", false},
		{"multiple wrappers", "sudo env FOO=bar timeout 5 ls", true},
		{"multiple wrappers rm", "sudo env FOO=bar timeout 5 rm x", false},
		{"bare env assignment", "FOO=bar ls", true},
		{"bare env assignment rm", "FOO=bar rm -rf x", false},

		// cd chains.
		{"cd and ls", "cd /tmp && ls", true},
		{"cd semicolon ls", "cd /tmp; ls", true},
		{"cd and rm", "cd /tmp && rm -rf x", false},
		{"cd semicolon rm", "cd /tmp; rm -rf x", false},
		{"cd bare", "cd /tmp", false},
		{"sudo cd and ls", "sudo cd /tmp && ls", true},

		// Compound commands.
		{"and both ro", "ls && grep foo", true},
		{"and one write", "ls && rm -rf x", false},
		{"and first write", "rm -rf x && ls", false},
		{"pipe ro", "git status | grep foo", true},
		{"pipe chain ro", "git status | grep foo | wc -l", true},
		{"or both ro", "git status || git log", true},
		{"semicolon both ro", "ls; pwd", true},
		{"semicolon one write", "ls; rm -rf x", false},
		{"pipe tee write", "ls | tee file", false},
		{"cd mid chain write", "ls && cd /tmp && rm x", false},

		// Redirection / write detection.
		{"redirect write", "ls > out.txt", false},
		{"redirect append", "ls >> out.txt", false},
		{"tee write", "cat file | tee out", false},
		{"dash o write", "gcc -o out main.c", false},
		{"output flag write", "tool --output out", false},
		{"install", "install pkg", false},
		{"uninstall", "uninstall pkg", false},
		{"mkdir", "mkdir foo", false},
		{"touch", "touch foo", false},
		{"mv", "mv a b", false},
		{"cp", "cp a b", false},
		{"chmod", "chmod +x f", false},
		{"chown", "chown u f", false},
		{"ln", "ln -s a b", false},
		{"wget", "wget https://example.com", false},

		// git mutations.
		{"git add", "git add .", false},
		{"git checkout", "git checkout main", false},
		{"git reset", "git reset --hard", false},
		{"git merge", "git merge x", false},
		{"git rebase", "git rebase x", false},
		{"git clean", "git clean -f", false},
		{"git rm", "git rm x", false},
		{"git clone", "git clone url", false},
		{"git switch", "git switch x", false},
		{"git restore", "git restore x", false},
		{"git pull", "git pull", false},
		{"git fetch", "git fetch", false},
		{"git stash bare", "git stash", false},
		{"git stash push", "git stash push", false},
		{"git stash pop", "git stash pop", false},
		{"git stash list ro", "git stash list", true},

		// Expanded read-only prefix table.
		{"docker ps", "docker ps -a", true},
		{"docker images", "docker images", true},
		{"docker inspect", "docker inspect foo", true},
		{"docker logs", "docker logs foo", true},
		{"docker stats", "docker stats", true},
		{"docker top", "docker top foo", true},
		{"docker version", "docker version", true},
		{"docker info", "docker info", true},
		{"docker compose ps", "docker compose ps", true},
		{"docker compose config", "docker compose config", true},
		{"docker run write", "docker run foo", false},
		{"docker build write", "docker build .", false},
		{"kubectl get", "kubectl get pods", true},
		{"kubectl describe", "kubectl describe pod x", true},
		{"kubectl logs", "kubectl logs pod", true},
		{"kubectl config view", "kubectl config view", true},
		{"kubectl explain", "kubectl explain pod", true},
		{"kubectl apply write", "kubectl apply -f x", false},
		{"kubectl delete write", "kubectl delete pod x", false},
		{"helm list", "helm list", true},
		{"helm status", "helm status foo", true},
		{"helm get", "helm get manifest foo", true},
		{"helm history", "helm history foo", true},
		{"helm install write", "helm install foo", false},
		{"terraform plan", "terraform plan", true},
		{"terraform state list", "terraform state list", true},
		{"terraform show", "terraform show", true},
		{"terraform output", "terraform output", true},
		{"terraform apply write", "terraform apply", false},
		{"aws describe", "aws ec2 describe-instances", true},
		{"aws get", "aws s3api get-bucket-location --bucket x", true},
		{"aws cp write", "aws s3 cp a b", false},
		{"aws run write", "aws ec2 run-instances", false},
		{"gh pr view", "gh pr view", true},
		{"gh issue list", "gh issue list", true},
		{"gh repo view", "gh repo view", true},
		{"gh status", "gh status", true},
		{"gh pr create write", "gh pr create", false},

		// Unknown commands must be false.
		{"empty", "", false},
		{"unknown", "someunknowncommand", false},
		{"make", "make all", false},
		{"kill", "kill 123", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsReadOnlyCommand(c.command); got != c.want {
				t.Errorf("IsReadOnlyCommand(%q) = %v, want %v", c.command, got, c.want)
			}
		})
	}
}

func TestIsDestructiveCommand(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    bool
	}{
		// File deletion.
		{"rm", "rm file.txt", true},
		{"rm rf", "rm -rf /tmp/x", true},
		{"rmdir", "rmdir foo", true},
		{"deltree", "deltree dir", true},
		{"del", "del file.txt", true},
		{"erase", "erase file.txt", true},
		{"truncate", "truncate -s 0 file", true},
		{"truncate table", "truncate table users", true},

		// Disk / machine level.
		{"dd", "dd if=/dev/zero of=/dev/sda", true},
		{"mkfs", "mkfs -t ext4 /dev/sda", true},
		{"shutdown", "shutdown now", true},
		{"reboot", "reboot", true},
		{"kill", "kill 123", true},
		{"killall", "killall foo", true},
		{"format", "format c:", true},

		// git history rewrites / force pushes.
		{"git reset hard", "git reset --hard", true},
		{"git reset hard ref", "git reset --hard HEAD~1", true},
		{"git push force", "git push --force origin main", true},
		{"git push f", "git push -f origin main", true},
		{"git push force with lease", "git push --force-with-lease origin main", true},
		{"git clean", "git clean -fd", true},
		{"git rebase", "git rebase main", true},
		{"bare rebase", "rebase main", true},
		{"git reflog delete", "git reflog delete HEAD@{1}", true},
		{"git reflog expire", "git reflog expire --all", true},
		{"git reflog drop", "git reflog drop HEAD@{1}", true},

		// SQL drops.
		{"drop table", "DROP TABLE users;", true},
		{"drop database", "drop database mydb", true},

		// Wrappers and chains must still be detected.
		{"sudo rm", "sudo rm -rf /", true},
		{"env rm", "env FOO=bar rm -rf x", true},
		{"cd and rm", "cd /tmp && rm -rf x", true},
		{"pipe rm", "ls | rm -rf x", true},

		// Non-destructive commands.
		{"ls", "ls -la", false},
		{"cat", "cat file.go", false},
		{"echo", "echo hello", false},
		{"git status", "git status", false},
		{"git log", "git log --oneline", false},
		{"git reset soft", "git reset", false},
		{"git push plain", "git push origin main", false},
		{"git reflog show", "git reflog show", false},
		{"docker ps", "docker ps -a", false},
		{"terraform plan", "terraform plan", false},
		{"empty", "", false},
		{"unknown", "someunknowncommand", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDestructiveCommand(c.command); got != c.want {
				t.Errorf("IsDestructiveCommand(%q) = %v, want %v", c.command, got, c.want)
			}
		})
	}
}

func TestIsReadOnlyTool(t *testing.T) {
	if !IsReadOnly("Read", nil) {
		t.Error("Read should be read-only")
	}
	if IsReadOnly("Write", nil) {
		t.Error("Write should not be read-only")
	}
	if !IsReadOnly("Bash", map[string]any{"command": "ls -la"}) {
		t.Error("Bash ls should be read-only")
	}
	if IsReadOnly("Bash", map[string]any{"command": "rm -rf x"}) {
		t.Error("Bash rm should not be read-only")
	}
	if !IsReadOnly("TodoWrite", nil) {
		t.Error("TodoWrite should be read-only (meta)")
	}
}
