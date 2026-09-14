package handler

import (
	"errors"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/pkg/sftp"

	"webssh/internal/sshmgr"
)

// ---------- 远程 SFTP ----------

type entryView struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	Owner   string `json:"owner"`
	Group   string `json:"group"`
	ModTime string `json:"mod_time"`
}

func entriesFromInfos(infos []os.FileInfo) []entryView {
	out := make([]entryView, 0, len(infos))
	for _, fi := range infos {
		e := entryView{
			Name: fi.Name(), IsDir: fi.IsDir(), Size: fi.Size(),
			Mode: fi.Mode().String(), ModTime: fi.ModTime().Format("2006-01-02 15:04:05"),
		}
		// 远程条目(SFTP)携带 *sftp.FileStat,本机条目携带 *syscall.Stat_t
		switch st := fi.Sys().(type) {
		case *sftp.FileStat:
			e.Owner = sshmgr.LookupUser(st.UID)
			e.Group = sshmgr.LookupGroup(st.GID)
		case *syscall.Stat_t:
			e.Owner = sshmgr.LookupUser(st.Uid)
			e.Group = sshmgr.LookupGroup(st.Gid)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func friendlyErr(err error, what string) string {
	var se *sftp.StatusError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, os.ErrNotExist), errors.As(err, &se) && se.Code == 2:
		return "路径不存在: " + what
	case errors.Is(err, os.ErrPermission), errors.As(err, &se) && se.Code == 3:
		return "权限不足: " + what
	default:
		return what + ": " + err.Error()
	}
}

func resolveRemotePath(s *sshmgr.Session, p string) (string, error) {
	if p == "" || p == "~" {
		return s.Home()
	}
	if strings.HasPrefix(p, "~/") {
		home, err := s.Home()
		if err == nil {
			return path.Join(home, strings.TrimPrefix(p, "~")), nil
		}
	}
	return path.Clean(p), nil
}

func (a *API) sftpList(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	sc, err := s.SFTP()
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	raw := c.Query("path")
	p, err := resolveRemotePath(s, raw)
	if err != nil {
		fail(c, 400, "解析主目录失败: "+err.Error())
		return
	}
	infos, err := sc.ReadDir(p)
	if err != nil {
		fail(c, 400, friendlyErr(err, raw))
		return
	}
	c.JSON(200, gin.H{"path": p, "entries": entriesFromInfos(infos)})
}

func remoteRemove(sc *sftp.Client, p string) error {
	fi, err := sc.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		items, err := sc.ReadDir(p)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := remoteRemove(sc, path.Join(p, it.Name())); err != nil {
				return err
			}
		}
		return sc.RemoveDirectory(p)
	}
	return sc.Remove(p)
}

func (a *API) sftpDelete(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	sc, err := s.SFTP()
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	p, err := resolveRemotePath(s, c.Query("path"))
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	if p == "/" || p == "." {
		fail(c, 400, "拒绝删除根目录")
		return
	}
	if err := remoteRemove(sc, p); err != nil {
		fail(c, 400, friendlyErr(err, c.Query("path")))
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (a *API) sftpMkdir(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	sc, err := s.SFTP()
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	var req mkdirReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	p, err := resolveRemotePath(s, strings.TrimSpace(req.Path))
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	if _, err := sc.Stat(p); err == nil {
		fail(c, 400, "目录已存在: "+p)
		return
	}
	if err := sc.Mkdir(p); err != nil {
		fail(c, 400, friendlyErr(err, p))
		return
	}
	c.JSON(200, gin.H{"ok": true, "path": p})
}

func (a *API) sftpRename(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	sc, err := s.SFTP()
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	var req renameReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	newName := strings.TrimSpace(req.NewName)
	if err := validName(newName); err != nil {
		fail(c, 400, err.Error())
		return
	}
	p, err := resolveRemotePath(s, strings.TrimSpace(req.Path))
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	np := path.Join(path.Dir(p), newName)
	if np == p {
		c.JSON(200, gin.H{"ok": true})
		return
	}
	if err := sc.Rename(p, np); err != nil {
		fail(c, 400, friendlyErr(err, p))
		return
	}
	c.JSON(200, gin.H{"ok": true, "path": np})
}
