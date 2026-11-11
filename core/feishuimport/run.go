package feishuimport

// Discover + import engine: walk roots → pending items → stage into docs.
// 发现＋导入引擎：遍历根→待导入条目→暂存文档。
import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"wittychat/core/docs"

	"github.com/google/uuid"
)

// Root describes one wiki space or drive folder. / 根。
type Root struct {
	Kind string `json:"kind"` // wiki_space | folder | doc | wiki_node
	ID   string `json:"id"`   // space_id | folder token | doc/node token
	Name string `json:"name"`
	Type string `json:"type,omitempty"` // doc|docx|sheet|bitable (kind=doc)
}

// Run does one budgeted chunk: discover phase (walk roots) then import
// pending items until the budget runs out (more=true).
// 一个预算块：先发现后导入，超预算返回 more=true。
func (s *Store) Run(ctx context.Context, jobID uuid.UUID, budget time.Duration) (bool, error) {
	j, err := s.loadJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	if j.Status != "running" {
		return false, nil
	}
	c, err := s.appCreds(ctx)
	if err != nil {
		_, _ = s.pool.Exec(ctx,
			`UPDATE feishu_import_jobs SET status='error', last_error=$2 WHERE id=$1`,
			jobID, "管理员尚未配置飞书应用")
		return false, nil
	}
	// Bad creds → job error, no retry (token() surfaces ErrBadCreds).
	if _, err := c.token(ctx); err != nil {
		if errors.Is(err, ErrBadCreds) {
			_, _ = s.pool.Exec(ctx,
				`UPDATE feishu_import_jobs SET status='error', last_error=$2 WHERE id=$1`,
				jobID, err.Error())
			return false, nil
		}
		return false, err
	}
	deadline := time.Now().Add(budget)
	var roots []Root
	if err := json.Unmarshal(j.Roots, &roots); err != nil {
		return false, err
	}
	// Discover phase — only until `discovered` is set; discover_done[] tracks
	// finished root indexes so a budget cut resumes, not restarts.
	if !j.Discovered {
		done := map[int]bool{}
		for _, i := range j.DiscoverDone {
			done[i] = true
		}
		for i, rt := range roots {
			if done[i] {
				continue
			}
			if err := s.discover(ctx, jobID, c, rt, rt.Name); err != nil {
				if errors.Is(err, ErrBadCreds) {
					_, _ = s.pool.Exec(ctx,
						`UPDATE feishu_import_jobs SET status='error',last_error=$2 WHERE id=$1`, jobID, err.Error())
					return false, nil
				}
				return false, err
			}
			done[i] = true
			idxs := make([]int, 0, len(done))
			for k := range done {
				idxs = append(idxs, k)
			}
			_, _ = s.pool.Exec(ctx,
				`UPDATE feishu_import_jobs SET discover_done=$2 WHERE id=$1`, jobID, idxs)
			if time.Now().After(deadline) {
				return true, nil
			}
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE feishu_import_jobs SET discovered=true WHERE id=$1`, jobID); err != nil {
			return false, err
		}
	}
	// Import phase: pending items until budget out.
	for {
		var it Item
		err := s.pool.QueryRow(ctx,
			`SELECT obj_type,obj_token,title,path,COALESCE(owner_open_id,'')
			 FROM feishu_import_items WHERE job_id=$1 AND status='pending'
			 ORDER BY obj_token LIMIT 1`, jobID).
			Scan(&it.ObjType, &it.ObjToken, &it.Title, &it.Path, &it.Owner)
		if err != nil { // no rows → done
			_ = s.setStatus(ctx, jobID, "done")
			return false, nil
		}
		if time.Now().After(deadline) {
			return true, nil
		}
		// re-check job status between items (pause/cancel)
		var st string
		if err := s.pool.QueryRow(ctx,
			`SELECT status FROM feishu_import_jobs WHERE id=$1`, jobID).Scan(&st); err == nil && st != "running" {
			return false, nil
		}
		s.importItem(ctx, j, c, it)
	}
}

// discover walks one root, upserting pending items. 发现一个根下的条目。
func (s *Store) discover(ctx context.Context, jobID uuid.UUID, c *fc, rt Root, path string) error {
	switch rt.Kind {
	case "wiki_space":
		return s.walkWiki(ctx, jobID, c, rt.ID, "", path)
	case "folder":
		return s.walkFolder(ctx, jobID, c, rt.ID, path)
	case "wiki_node":
		// resolve the node → obj_token/obj_type/title/owner, then walk children
		var out struct {
			Node struct {
				ObjToken string `json:"obj_token"`
				ObjType  string `json:"obj_type"`
				Title    string `json:"title"`
				Owner    string `json:"owner"`
				SpaceID  string `json:"space_id"`
				HasChild bool   `json:"has_child"`
			} `json:"node"`
		}
		if err := c.get(ctx, "/open-apis/wiki/v2/spaces/get_node?token="+rt.ID, &out); err != nil {
			return err
		}
		n := out.Node
		s.upsertItem(ctx, jobID, Item{
			ObjType: n.ObjType, ObjToken: n.ObjToken,
			Title: n.Title, Path: rt.Name, Owner: n.Owner,
		})
		if n.HasChild {
			return s.walkWiki(ctx, jobID, c, n.SpaceID, rt.ID, rt.Name+"/"+n.Title)
		}
		return nil
	case "doc":
		// single document — owner/title via drive metas batch_query
		var out struct {
			Metas []struct {
				DocToken string `json:"doc_token"`
				Title    string `json:"title"`
				OwnerID  string `json:"owner_id"`
			} `json:"metas"`
		}
		body, _ := json.Marshal(map[string]any{
			"request_docs": []map[string]string{{"doc_token": rt.ID, "doc_type": rt.Type}},
			"with_url":     false,
		})
		if err := c.post(ctx, "/open-apis/drive/v1/metas/batch_query", body, &out); err != nil {
			return err
		}
		m := struct {
			Title, OwnerID string
		}{Title: rt.Name}
		if len(out.Metas) > 0 {
			m.Title = out.Metas[0].Title
			m.OwnerID = out.Metas[0].OwnerID
		}
		s.upsertItem(ctx, jobID, Item{
			ObjType: rt.Type, ObjToken: rt.ID,
			Title: m.Title, Path: "", Owner: m.OwnerID,
		})
		return nil
	default:
		return nil
	}
}

func (s *Store) upsertItem(ctx context.Context, jobID uuid.UUID, it Item) {
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO feishu_import_items (job_id,obj_type,obj_token,title,path,owner_open_id,status)
		 VALUES ($1,$2,$3,$4,$5,$6,'pending')
		 ON CONFLICT (job_id,obj_type,obj_token) DO UPDATE
		 SET title=EXCLUDED.title,path=EXCLUDED.path,owner_open_id=EXCLUDED.owner_open_id,
		     updated_at=now()`,
		jobID, it.ObjType, it.ObjToken, it.Title, it.Path, it.Owner)
}

func (s *Store) walkWiki(ctx context.Context, jobID uuid.UUID, c *fc, spaceID, parentToken, path string) error {
	var out struct {
		Items []struct {
			NodeToken string `json:"node_token"`
			ObjToken  string `json:"obj_token"`
			ObjType   string `json:"obj_type"`
			Title     string `json:"title"`
			NodeType  string `json:"node_type"`
			Owner     string `json:"owner"`
			HasChild  bool   `json:"has_child"`
		} `json:"items"`
		HasMore   bool   `json:"has_more"`
		PageToken string `json:"page_token"`
	}
	tok := ""
	for {
		q := fmt.Sprintf("/open-apis/wiki/v2/spaces/%s/nodes?page_size=50&page_token=%s", spaceID, tok)
		if parentToken != "" {
			q += "&parent_node_token=" + parentToken
		}
		if err := c.get(ctx, q, &out); err != nil {
			return err
		}
		for _, it := range out.Items {
			if it.NodeType == "shortcut" {
				continue
			}
			s.upsertItem(ctx, jobID, Item{
				ObjType: it.ObjType, ObjToken: it.ObjToken,
				Title: it.Title, Path: path, Owner: it.Owner,
			})
			if it.HasChild {
				if err := s.walkWiki(ctx, jobID, c, spaceID, it.NodeToken, path+"/"+it.Title); err != nil {
					return err
				}
			}
		}
		if !out.HasMore {
			return nil
		}
		tok = out.PageToken
	}
}

func (s *Store) walkFolder(ctx context.Context, jobID uuid.UUID, c *fc, token, path string) error {
	var out struct {
		Files []struct {
			Token   string `json:"token"`
			Type    string `json:"type"`
			Name    string `json:"name"`
			OwnerID string `json:"owner_id"`
		} `json:"files"`
		HasMore bool `json:"has_more"`
	}
	q := fmt.Sprintf("/open-apis/drive/v1/files?folder_token=%s&page_size=200", token)
	if err := c.get(ctx, q, &out); err != nil {
		return err
	}
	for _, f := range out.Files {
		if f.Type == "shortcut" {
			continue
		}
		if f.Type == "folder" {
			if err := s.walkFolder(ctx, jobID, c, f.Token, path+"/"+f.Name); err != nil {
				return err
			}
			continue
		}
		s.upsertItem(ctx, jobID, Item{
			ObjType: f.Type, ObjToken: f.Token,
			Title: f.Name, Path: path, Owner: f.OwnerID,
		})
	}
	return nil
}

// importItem converts one item → staged doc(s). 导入单条。
func (s *Store) importItem(ctx context.Context, j *Job, c *fc, it Item) {
	fail := func(err error) {
		_, _ = s.pool.Exec(ctx,
			`UPDATE feishu_import_items SET status='failed',error=$3,updated_at=now() WHERE job_id=$1 AND obj_type=$2 AND obj_token=$4`,
			j.ID, it.ObjType, err.Error(), it.ObjToken)
	}
	ext, ok := map[string]string{"docx": "docx", "doc": "docx", "sheet": "xlsx", "bitable": "xlsx"}[it.ObjType]
	if !ok {
		_, _ = s.pool.Exec(ctx,
			`UPDATE feishu_import_items SET status='skipped',error=$3,updated_at=now() WHERE job_id=$1 AND obj_type=$2 AND obj_token=$4`,
			j.ID, it.ObjType, "不支持的类型："+it.ObjType, it.ObjToken)
		return
	}
	// self-mode ownership check FIRST — skip before spending an export on it
	if j.Mode == "self" {
		email := c.emailFor(ctx, it.Owner)
		if !j.creatorOwns(email) {
			// Unknown owner fails closed, but as `failed` so a re-start retries
			// it once the app gets contact access; a known other owner is final.
			// 属主未知按失败处理（补权限后重跑可重试）；确属他人则跳过。
			st, msg := "failed", "无法确认文档所有者（应用可能缺少通讯录读取权限）"
			if email != "" {
				st, msg = "skipped", "不是你的文档（所有者："+email+"）"
			}
			_, _ = s.pool.Exec(ctx,
				`UPDATE feishu_import_items SET status=$5,error=$3,updated_at=now() WHERE job_id=$1 AND obj_type=$2 AND obj_token=$4`,
				j.ID, it.ObjType, msg, it.ObjToken, st)
			return
		}
	}
	// export task → ticket → poll → download
	ticket, err := c.export(ctx, it.ObjToken, it.ObjType, ext)
	if err != nil {
		fail(err)
		return
	}
	tmp, err := os.CreateTemp("", "feishu-*."+ext)
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := c.downloadTo(ctx, ticket, tmp); err != nil {
		_ = tmp.Close()
		fail(err)
		return
	}
	_ = tmp.Close()
	// owner mapping
	owner := j.CreatedBy
	email := c.emailFor(ctx, it.Owner)
	if j.Mode != "self" && email != "" {
		var uid uuid.UUID
		// primary email OR a mail_addresses entry, active users only
		if err := s.pool.QueryRow(ctx,
			`SELECT u.id FROM users u WHERE lower(u.email)=lower($1) AND u.status='active'
			 UNION SELECT a.user_id FROM mail_addresses a JOIN users u2 ON u2.id=a.user_id
			 WHERE lower(a.address)=lower($1) AND u2.status='active' LIMIT 1`, email).
			Scan(&uid); err == nil {
			owner = uid
		}
	}
	// store the raw file as an object
	fh, err := os.Open(tmp.Name())
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = fh.Close() }()
	obj, _, err := s.objects.Put(ctx, owner, uuid.Nil, "doc", it.Title+"."+ext,
		"application/octet-stream", fh, maxFileBytes)
	if err != nil {
		fail(err)
		return
	}
	title := docs.NormTitle(it.Title)
	if it.Path != "" {
		title = docs.NormTitle(it.Path + "/" + it.Title)
	}
	var created []uuid.UUID
	if ext == "docx" {
		d, err := s.docs.Create(ctx, owner, title, "doc", nil)
		if err != nil {
			fail(err)
			return
		}
		created = append(created, d.ID)
		if err := s.stage(ctx, j.ID, it, d.ID, obj.ID, "docx", ""); err != nil {
			s.cleanup(ctx, created)
			fail(err)
			return
		}
	} else { // xlsx → one sheet doc per worksheet
		names := sheetNames(tmp.Name())
		if len(names) == 0 {
			names = []string{"Sheet1"}
		}
		for i, name := range names {
			t := title
			if len(names) > 1 {
				t = docs.NormTitle(title + " · " + name)
			}
			d, err := s.docs.Create(ctx, owner, t, "sheet", nil)
			if err != nil {
				s.cleanup(ctx, created)
				fail(err)
				return
			}
			created = append(created, d.ID)
			if err := s.stage(ctx, j.ID, it, d.ID, obj.ID, "xlsx", name); err != nil {
				s.cleanup(ctx, created)
				fail(err)
				return
			}
			_ = i
		}
	}
	_, _ = s.pool.Exec(ctx,
		`UPDATE feishu_import_items SET status='done',doc_ids=$4,updated_at=now() WHERE job_id=$1 AND obj_type=$2 AND obj_token=$3`,
		j.ID, it.ObjType, it.ObjToken, created)
}

// stage links the object + stages the file payload. 暂存文件载荷。
func (s *Store) stage(ctx context.Context, jobID uuid.UUID, it Item, docID, objID uuid.UUID, format, sheet string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO doc_objects (doc_id,object_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, docID, objID); err != nil {
		return err
	}
	payload := map[string]any{"type": "file", "object_id": objID.String(), "format": format}
	if sheet != "" {
		payload["sheet"] = sheet
	}
	b, _ := json.Marshal(payload)
	return s.docs.Stage(ctx, docID, b, "feishu:"+it.ObjType+":"+it.ObjToken)
}

func (s *Store) cleanup(ctx context.Context, ids []uuid.UUID) {
	for _, id := range ids {
		_ = s.docs.Delete(ctx, id, uuid.Nil)
	}
}

// export creates an export task and polls until done. 导出任务→轮询。
func (c *fc) export(ctx context.Context, token, typ, ext string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"file_extension": ext, "token": token, "type": typ,
	})
	b, err := c.do(ctx, http.MethodPost, "/open-apis/drive/v1/export_tasks", body, true)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			Ticket string `json:"ticket"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Data.Ticket == "" {
		return "", fmt.Errorf("export create failed")
	}
	time.Sleep(1100 * time.Millisecond) // ≤1 export/s
	deadline := time.Now().Add(60 * time.Second)
	for {
		var st struct {
			Result struct {
				Status    int    `json:"job_status"`
				ErrorMsg  string `json:"job_error_msg"`
				FileToken string `json:"file_token"`
			} `json:"result"`
		}
		if err := c.get(ctx, fmt.Sprintf("/open-apis/drive/v1/export_tasks/%s?token=%s", out.Data.Ticket, token), &st); err != nil {
			return "", err
		}
		switch st.Result.Status {
		case 0:
			return st.Result.FileToken, nil
		case 1, 2:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("export timeout")
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(1 * time.Second):
			}
		case 109, 110:
			return "", ErrForbidden
		default:
			return "", fmt.Errorf("export status %d: %s", st.Result.Status, st.Result.ErrorMsg)
		}
	}
}

// downloadTo streams the export straight into the file (no full buffer).
// 流式下载进临时文件；超限→导出文件过大。
func (c *fc) downloadTo(ctx context.Context, fileToken string, w *os.File) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/open-apis/drive/v1/export_tasks/file/"+fileToken+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 403 {
		return ErrForbidden
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("download → %d", resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return err
	}
	if n > maxFileBytes {
		return fmt.Errorf("导出文件过大")
	}
	return nil
}

// sheetNames reads worksheet names from xl/workbook.xml. 读工作表名。
func sheetNames(path string) []string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != "workbook.xml" || !strings.HasPrefix(f.Name, "xl/") {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return nil
		}
		var wb struct {
			Sheets []struct {
				Name string `xml:"name,attr"`
			} `xml:"sheets>sheet"`
		}
		err = xml.NewDecoder(r).Decode(&wb)
		_ = r.Close()
		if err != nil {
			return nil
		}
		var out []string
		for _, sh := range wb.Sheets {
			out = append(out, sh.Name)
		}
		return out
	}
	return nil
}

// ParseFeishuURL maps a Feishu link to a discoverable root. 解析飞书链接→根。
func ParseFeishuURL(raw string) (Root, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return Root{}, false
	}
	if !strings.HasSuffix(u.Host, "feishu.cn") && !strings.HasSuffix(u.Host, "larksuite.com") {
		return Root{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	tok := ""
	if len(parts) >= 2 {
		tok = parts[len(parts)-1]
	}
	if tok == "" {
		return Root{}, false
	}
	switch parts[0] {
	case "docx":
		return Root{Kind: "doc", Type: "docx", ID: tok, Name: tok}, true
	case "docs":
		return Root{Kind: "doc", Type: "doc", ID: tok, Name: tok}, true
	case "sheets":
		return Root{Kind: "doc", Type: "sheet", ID: tok, Name: tok}, true
	case "base":
		return Root{Kind: "doc", Type: "bitable", ID: tok, Name: tok}, true
	case "wiki":
		return Root{Kind: "wiki_node", ID: tok, Name: tok}, true
	case "drive":
		if len(parts) >= 3 && parts[1] == "folder" {
			return Root{Kind: "folder", ID: parts[2], Name: parts[2]}, true
		}
	}
	return Root{}, false
}
