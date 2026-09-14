package handler

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// ---------- 本机文件 ----------

func localDefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/"
	}
	return home
}

func (a *API) localList(c *gin.Context) {
	p := strings.TrimSpace(c.Query("path"))
	if p == "" || p == "~" {
		p = localDefaultPath()
	}
	p = filepath.Clean(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		fail(c, 400, friendlyErr(err, p))
		return
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err == nil {
			infos = append(infos, fi)
		}
	}
	c.JSON(200, gin.H{"path": p, "entries": entriesFromInfos(infos)})
}

func localRemove(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		items, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := localRemove(filepath.Join(p, it.Name())); err != nil {
				return err
			}
		}
		return os.Remove(p)
	}
	return os.Remove(p)
}

func (a *API) localDelete(c *gin.Context) {
	p := strings.TrimSpace(c.Query("path"))
	if p == "" {
		fail(c, 400, "缺少 path 参数")
		return
	}
	p = filepath.Clean(p)
	if p == "/" {
		fail(c, 400, "拒绝删除根目录")
		return
	}
	if err := localRemove(p); err != nil {
		fail(c, 400, friendlyErr(err, p))
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (a *API) localMkdir(c *gin.Context) {
	var req mkdirReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	p := filepath.Clean(strings.TrimSpace(req.Path))
	if p == "" || p == "." {
		fail(c, 400, "路径不能为空")
		return
	}
	if _, err := os.Stat(p); err == nil {
		fail(c, 400, "目录已存在: "+p)
		return
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		fail(c, 400, friendlyErr(err, p))
		return
	}
	c.JSON(200, gin.H{"ok": true, "path": p})
}

func (a *API) localRename(c *gin.Context) {
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
	p := filepath.Clean(strings.TrimSpace(req.Path))
	np := filepath.Join(filepath.Dir(p), newName)
	if np == p {
		c.JSON(200, gin.H{"ok": true})
		return
	}
	if _, err := os.Stat(np); err == nil {
		fail(c, 400, "目标名称已存在: "+newName)
		return
	}
	if err := os.Rename(p, np); err != nil {
		fail(c, 400, friendlyErr(err, p))
		return
	}
	c.JSON(200, gin.H{"ok": true, "path": np})
}
