// Package core holds DB access and domain logic shared by server and worker.
// Desktop must never import it (enforced by depguard).
//
// core 包存放 server 与 worker 共用的数据库访问与领域逻辑。
// desktop 不得 import 本包（depguard 强制）。
package core
