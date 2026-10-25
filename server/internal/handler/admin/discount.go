package admin

// 优惠码 + 文件夹（对齐 Admin\DiscountCodeController / DiscountCodeFolderController）。
//
// 分组口径（prompts/discount-folders-20261023）：
//   - 未传 folder：list 只出未归档码（folder_id = null）
//   - 传 folder=id：list 只出该文件夹内的码
//   - folders 始终返回全部文件夹，code_count 按当前筛选条件计数；
//     有筛选条件且该文件夹内有命中项时标 matched。
//
// 删除文件夹的默认语义：把里面所有码的 folder_id 置 null（回到「未归档」），码一条都不删。

import (
	"context"
	"crypto/rand"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// 生成码用的字符集与长度（对齐 DiscountCode 模型常量）。
//
// 码体字符集：Base32 去掉易混的 0 O 1 I L；长度 8 位。
const (
	codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	codeLength   = 8
	// discountPrefixRe 对齐 prefix 规则 regex:/^[A-Z0-9]{1,10}$/i。
	discountPrefixRe = `^[A-Za-z0-9]{1,10}$`
)

// listDiscountCodes 对齐 GET /admin-api/discount-codes。
func (h *Handler) listDiscountCodes(c *gin.Context) {
	// 校验顺序与 PHP 一致：source → status → user → folder → page → per_page。
	// query 参数在 PHP 侧永远是**字符串**（$_GET），Laravel 的 integer/numeric 规则接受
	// 数字字符串。所以这里保持字符串，不要转成 jsonNumber —— 否则 string 规则会误判
	// （金样本 discount_user_search 就是 `user=zzz` 被当成非字符串而 500）。
	input := map[string]any{}
	for _, k := range []string{"source", "status", "user", "folder", "page", "per_page"} {
		if v := queryString(c, k); v != nil {
			input[k] = *v
		}
	}

	v := newValidator(ctxOf(c), input).
		existsChecker(h.existsChecker()).
		messages(map[string]string{"folder.exists": "文件夹不存在"})
	v.field("source", "sometimes", "nullable", "in:invite,redeem,admin")
	v.field("status", "sometimes", "nullable", "in:active,used_up,expired,refreshed")
	v.field("user", "sometimes", "nullable", "string", "max:255")
	v.field("folder", "sometimes", "nullable", "integer", "exists:discount_code_folders,id")
	v.field("page", "sometimes", "integer", "min:1")
	v.field("per_page", "sometimes", "integer", "min:1", "max:100")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	perPage := int64(20)
	if val, ok := input["per_page"]; ok && val != nil {
		perPage = inputInt(val)
	}
	page := int64(1)
	if val, ok := input["page"]; ok && val != nil {
		page = inputInt(val)
	}
	var folderID *uint64
	if val, ok := input["folder"]; ok && val != nil {
		id := uint64(inputInt(val))
		folderID = &id
	}

	filter := DiscountFilter{}
	if val, ok := input["source"]; ok && val != nil {
		s, _ := inputString(val)
		filter.Source = &s
	}
	if val, ok := input["status"]; ok && val != nil {
		s, _ := inputString(val)
		filter.Status = &s
	}
	if val, ok := input["user"]; ok && val != nil {
		s, _ := inputString(val)
		filter.User = &s
	}

	rows, total, err := h.store.DiscountPage(ctxOf(c), filter, folderID, page, perPage)
	if err != nil {
		h.failServer(c, "查询优惠码", err)
		return
	}
	folderCounts, err := h.store.DiscountFolderCounts(ctxOf(c), filter)
	if err != nil {
		h.failServer(c, "查询优惠码文件夹", err)
		return
	}

	// 有没有生效的筛选条件：决定 folders 里的 matched 是否有意义。
	filtering := filter.Source != nil || filter.Status != nil || filter.User != nil

	list := make(jsArr, 0, len(rows))
	for i := range rows {
		list = append(list, presentDiscount(&rows[i]))
	}
	folders := make(jsArr, 0, len(folderCounts))
	for _, f := range folderCounts {
		folders = append(folders, jo(
			"id", f.ID,
			"name", f.Name,
			"code_count", f.CodeCount,
			"matched", filtering && f.CodeCount > 0,
		))
	}

	h.success(c, jo(
		"list", list,
		"folders", folders,
		"total", total,
		"page", page,
		"per_page", perPage,
	))
}

// createDiscountCodes 对齐 POST /admin-api/discount-codes（支持批量生成 N 个）。
func (h *Handler) createDiscountCodes(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input).
		existsChecker(h.existsChecker())
	v.field("discount", "required", "numeric", "gt:0", "lte:1")
	v.field("max_uses", "nullable", "integer", "min:1")
	v.field("max_uses_per_user", "nullable", "integer", "min:1")
	v.field("expires_at", "nullable", "date")
	v.field("note", "nullable", "string", "max:255")
	v.field("plan_id", "nullable", "integer", "exists:plans,id")
	v.field("count", "nullable", "integer", "min:1", "max:500")
	v.field("prefix", "sometimes", "nullable", "string", "regex:/"+discountPrefixRe+"/i")
	v.field("folder_id", "nullable", "integer", "exists:discount_code_folders,id")
	v.field("folder_name", "sometimes", "nullable", "string", "max:60")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	prefix := "PRO"
	if val, ok := input["prefix"]; ok && val != nil {
		s, _ := inputString(val)
		prefix = phpStrToUpper(phpTrimSpaceOnly(s))
		if prefix == "" {
			prefix = "PRO"
		}
	}

	folderID, err := h.resolveFolderID(ctxOf(c), input)
	if err != nil {
		h.failServer(c, "解析优惠码文件夹", err)
		return
	}

	count := int64(1)
	if val, ok := input["count"]; ok && val != nil {
		count = inputInt(val)
	}
	codes, err := h.generateCodes(ctxOf(c), prefix, count)
	if err != nil {
		h.failServer(c, "生成优惠码", err)
		return
	}

	var maxUses, maxUsesPerUser int64 = 1, 1
	if val, ok := input["max_uses"]; ok && val != nil {
		maxUses = inputInt(val)
	}
	if val, ok := input["max_uses_per_user"]; ok && val != nil {
		maxUsesPerUser = inputInt(val)
	}

	if len(codes) > 0 {
		now := h.now().UTC().Truncate(time.Second)
		discount, _ := decimalString(input["discount"], 2)
		var expiresAt *time.Time
		if val, ok := input["expires_at"]; ok && val != nil {
			s, _ := inputString(val)
			t := parseDateTimeString(s)
			expiresAt = &t
		}
		var note *string
		if val, ok := input["note"]; ok {
			note = ptrStringOrNil(val)
		}
		var planID *uint64
		if val, ok := input["plan_id"]; ok && val != nil {
			id := uint64(inputInt(val))
			planID = &id
		}

		rows := make([]NewDiscountCode, 0, len(codes))
		for _, code := range codes {
			rows = append(rows, NewDiscountCode{
				Code:           code,
				Discount:       discount,
				MaxUses:        maxUses,
				MaxUsesPerUser: maxUsesPerUser,
				ExpiresAt:      expiresAt,
				Note:           note,
				PlanID:         planID,
				FolderID:       folderID,
				CreatedAt:      now,
			})
		}
		// 整批一个事务：中途失败不能留下「生成了 300 个、接口报错」的半批数据。
		if err := h.store.InsertDiscountCodes(ctxOf(c), rows); err != nil {
			h.failServer(c, "写入优惠码", err)
			return
		}
	}

	codeArr := make(jsArr, 0, len(codes))
	for _, code := range codes {
		codeArr = append(codeArr, code)
	}
	h.success(c, jo(
		"count", len(codes),
		"codes", codeArr,
		"folder_id", nullableUint(folderID),
	), "生成成功")
}

// updateDiscountCode 对齐 PUT /admin-api/discount-codes/{code}。
//
// 路由参数既支持主键 id 也支持码本身。
func (h *Handler) updateDiscountCode(c *gin.Context) {
	raw := c.Param("code")
	row, err := h.store.DiscountByIDOrCode(ctxOf(c), raw)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询优惠码", err)
		return
	}
	if row == nil {
		h.fail(c, "优惠码不存在", 404)
		return
	}

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input).
		existsChecker(h.existsChecker())
	v.field("discount", "sometimes", "numeric", "gt:0", "lte:1")
	v.field("max_uses", "sometimes", "integer", "min:1")
	v.field("max_uses_per_user", "sometimes", "integer", "min:1")
	v.field("expires_at", "sometimes", "nullable", "date")
	v.field("note", "sometimes", "nullable", "string", "max:255")
	v.field("status", "sometimes", "in:active,used_up,expired,refreshed")
	v.field("plan_id", "sometimes", "nullable", "integer", "exists:plans,id")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	upd := DiscountUpdate{}
	if val, ok := input["discount"]; ok && val != nil {
		s, _ := decimalString(val, 2)
		upd.Discount = &s
	}
	if val, ok := input["max_uses"]; ok && val != nil {
		n := inputInt(val)
		// 次数下限保护：used_count 是已经发生的真实消耗，把上限改到它之下会让数据自相矛盾。
		if n < row.UsedCount {
			h.fail(c, "使用次数上限不能小于已使用次数（当前已用 "+strconv.FormatInt(row.UsedCount, 10)+" 次）", 400)
			return
		}
		upd.MaxUses = &n
	}
	if val, ok := input["max_uses_per_user"]; ok && val != nil {
		n := inputInt(val)
		upd.MaxUsesPerUser = &n
	}
	if val, ok := input["expires_at"]; ok {
		upd.ExpiresAtSet = true
		if val != nil {
			s, _ := inputString(val)
			t := parseDateTimeString(s)
			upd.ExpiresAt = &t
		}
	}
	if val, ok := input["note"]; ok {
		upd.NoteSet = true
		upd.Note = ptrStringOrNil(val)
	}
	if val, ok := input["status"]; ok && val != nil {
		s, _ := inputString(val)
		upd.Status = &s
	}
	if val, ok := input["plan_id"]; ok {
		upd.PlanIDSet = true
		if val != nil {
			id := uint64(inputInt(val))
			upd.PlanID = &id
		}
	}

	if err := h.store.UpdateDiscount(ctxOf(c), row.ID, upd); err != nil {
		h.failServer(c, "保存优惠码", err)
		return
	}
	h.success(c, nil, "保存成功")
}

// deleteDiscountCode 对齐 DELETE /admin-api/discount-codes/{code}。
func (h *Handler) deleteDiscountCode(c *gin.Context) {
	raw := c.Param("code")
	row, err := h.store.DiscountByIDOrCode(ctxOf(c), raw)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询优惠码", err)
		return
	}
	if row == nil {
		h.fail(c, "优惠码不存在", 404)
		return
	}
	if err := h.store.DeleteDiscount(ctxOf(c), row.ID); err != nil {
		h.failServer(c, "删除优惠码", err)
		return
	}
	h.success(c, nil, "已删除")
}

// batchDeleteDiscountCodes 对齐 POST /admin-api/discount-codes/batch-delete（body: {ids}）。
//
// - 逐条删，单条失败不影响其余，按 id 回报 failed
// - 不存在的 id 归入 failed(not_found)，不静默吞
func (h *Handler) batchDeleteDiscountCodes(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("ids", "required", "array", "min:1", "max:500")
	v.field("ids.*", "integer", "min:1")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	rawIDs, _ := input["ids"].([]any)
	ids := make([]uint64, 0, len(rawIDs))
	seen := map[uint64]struct{}{}
	for _, raw := range rawIDs {
		id := uint64(inputInt(raw))
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	targets, err := h.store.DiscountsByIDs(ctxOf(c), ids)
	if err != nil {
		h.failServer(c, "查询优惠码", err)
		return
	}

	deleted := make(jsArr, 0, len(ids))
	failed := make(jsArr, 0)
	for _, id := range ids {
		if _, ok := targets[id]; !ok {
			failed = append(failed, jo("id", id, "reason", "not_found"))
			continue
		}
		if err := h.store.DeleteDiscount(ctxOf(c), id); err != nil {
			failed = append(failed, jo("id", id, "reason", err.Error()))
			continue
		}
		deleted = append(deleted, id)
	}

	h.success(c, jo("deleted", deleted, "failed", failed))
}

// ---- 文件夹 ----

// createFolder 对齐 POST /admin-api/discount-code-folders。
func (h *Handler) createFolder(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("name", "required", "string", "max:60")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}
	name, _ := inputString(input["name"])
	f, err := h.store.FolderCreate(ctxOf(c), phpTrimSpaceOnly(name))
	if err != nil {
		h.failServer(c, "创建优惠码文件夹", err)
		return
	}
	h.success(c, jo("id", f.ID, "name", f.Name, "code_count", 0), "已创建")
}

// updateFolder 对齐 PUT /admin-api/discount-code-folders/{folder}（改名）。
func (h *Handler) updateFolder(c *gin.Context) {
	// 路由参数是 {folder} 字符串 + 控制器里手动 resolveFolder（不是隐式模型绑定），
	// 所以不存在时回的是业务文案「文件夹不存在」404，而不是「接口不存在」。
	folder, ok := h.loadFolder(c)
	if !ok {
		return
	}
	id := folder.ID

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("name", "required", "string", "max:60")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}
	name, _ := inputString(input["name"])
	if err := h.store.FolderUpdate(ctxOf(c), id, phpTrimSpaceOnly(name)); err != nil {
		h.failServer(c, "保存优惠码文件夹", err)
		return
	}
	h.success(c, nil, "保存成功")
}

// deleteFolder 对齐 DELETE /admin-api/discount-code-folders/{folder}[?with_codes=1]。
//
// 默认：里面所有码退回「未归档」，码一条都不删。
// with_codes=1：连同文件夹内的码一起删除（需要二次确认，前端负责）。
func (h *Handler) deleteFolder(c *gin.Context) {
	folder, ok := h.loadFolder(c)
	if !ok {
		return
	}
	id := folder.ID

	if inputBool(queryRaw(c, "with_codes")) {
		codes, err := h.store.DiscountCodesInFolder(ctxOf(c), id)
		if err != nil {
			h.failServer(c, "查询文件夹内优惠码", err)
			return
		}
		deleted := 0
		failed := make(jsArr, 0)
		for i := range codes {
			if err := h.store.DeleteDiscount(ctxOf(c), codes[i].ID); err != nil {
				failed = append(failed, jo("id", codes[i].ID, "reason", err.Error()))
				continue
			}
			deleted++
		}
		// 没删掉的不能留成孤儿 folder_id：退回未归档。
		if len(failed) > 0 {
			if err := h.store.DetachCodesFromFolder(ctxOf(c), id); err != nil {
				h.failServer(c, "摘除文件夹内优惠码", err)
				return
			}
		}
		if err := h.store.FolderDelete(ctxOf(c), folder.ID); err != nil {
			h.failServer(c, "删除优惠码文件夹", err)
			return
		}

		var msg string
		if len(failed) == 0 {
			msg = "已删除文件夹及其中 " + strconv.Itoa(deleted) + " 个优惠码"
		} else {
			msg = "已删除文件夹，" + strconv.Itoa(deleted) + " 个码已删，" +
				strconv.Itoa(len(failed)) + " 个删除失败已转为未归档"
		}
		h.success(c, jo("deleted_codes", deleted, "failed", failed), msg)
		return
	}

	// 先摘码再删文件夹：删到一半失败时，码只会多出一批「未归档」，
	// 不会出现指向已删文件夹的孤儿 folder_id。
	if err := h.store.DetachCodesFromFolder(ctxOf(c), folder.ID); err != nil {
		h.failServer(c, "摘除文件夹内优惠码", err)
		return
	}
	if err := h.store.FolderDelete(ctxOf(c), folder.ID); err != nil {
		h.failServer(c, "删除优惠码文件夹", err)
		return
	}
	h.success(c, nil, "已删除文件夹，其中优惠码已转为未归档")
}

// ---- 内部 ----

// resolveFolderID 对齐 resolveFolderId：folder_id 优先；否则按 folder_name 找，不存在就建。
func (h *Handler) resolveFolderID(ctx context.Context, input map[string]any) (*uint64, error) {
	if val, ok := input["folder_id"]; ok && val != nil {
		id := uint64(inputInt(val))
		if id > 0 {
			return &id, nil
		}
	}
	name := ""
	if val, ok := input["folder_name"]; ok && val != nil {
		s, _ := inputString(val)
		name = phpTrimSpaceOnly(s)
	}
	if name == "" {
		return nil, nil
	}
	f, err := h.store.FolderByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if f != nil {
		return &f.ID, nil
	}
	created, err := h.store.FolderCreate(ctx, name)
	if err != nil {
		return nil, err
	}
	return &created.ID, nil
}

// generateCodes 对齐 DiscountCode::generateCodes：一次查库批量去重，冲突项重生成。
//
// 最多重试 20 轮（与 PHP 的 guard 一致）；极端情况下返回不足 count 个。
func (h *Handler) generateCodes(ctx context.Context, prefix string, count int64) ([]string, error) {
	if count <= 0 {
		return nil, nil
	}
	prefix = phpStrToUpper(prefix)
	codes := make([]string, 0, count)
	seen := make(map[string]struct{}, count)

	for guard := 0; len(codes) < int(count) && guard < 20; guard++ {
		need := int(count) - len(codes)
		batch := make([]string, 0, need)
		batchSeen := make(map[string]struct{}, need)
		for i := 0; i < need; i++ {
			code := prefix + randomCodeBody()
			if _, dup := batchSeen[code]; dup {
				continue
			}
			batchSeen[code] = struct{}{}
			batch = append(batch, code)
		}
		if len(batch) == 0 {
			continue
		}
		taken, err := h.store.ExistingCodes(ctx, batch)
		if err != nil {
			return nil, err
		}
		for _, code := range batch {
			if _, exists := taken[code]; exists {
				continue
			}
			if _, dup := seen[code]; dup {
				continue
			}
			seen[code] = struct{}{}
			codes = append(codes, code)
			if len(codes) >= int(count) {
				break
			}
		}
	}
	return codes, nil
}

// randomCodeBody 取一个 8 位码体：逐字节取随机数，尾部余数丢弃重取（每字符等概率）。
func randomCodeBody() string {
	size := len(codeAlphabet)
	limit := (256 / size) * size
	out := make([]byte, 0, codeLength)
	buf := make([]byte, 1)
	for len(out) < codeLength {
		if _, err := rand.Read(buf); err != nil {
			panic("admin: 生成优惠码随机数失败: " + err.Error())
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, codeAlphabet[int(buf[0])%size])
	}
	return string(out)
}

// parseDateTimeString 解析 expires_at：对齐 Carbon::parse 的常见形态（YYYY-MM-DD[ HH:MM[:SS]]）。
//
// 时区口径见 data-contract §4：库里的 datetime 字面量按 UTC 解释、不做换算。
// 带显式偏移（+08:00 / Z）的输入会先换算到 UTC 再落库，与 Carbon 一致。
func parseDateTimeString(raw string) time.Time {
	s := phpTrimSpaceOnly(raw)
	layouts := []string{
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Truncate(time.Second)
		}
	}
	// 兜底：解析失败时按零值处理（date 规则已在前面拦掉非法输入）。
	return time.Time{}
}

// presentDiscount 对齐控制器里 map 出来的字段顺序。
func presentDiscount(r *DiscountRow) *jsObj {
	return jo(
		"id", r.ID,
		"code", r.Code,
		"source", r.Source,
		"user_id", nullableUint(r.UserID),
		"user_email", nullableStr(r.UserEmail),
		"discount", phpFloatOf(r.Discount),
		"max_uses", r.MaxUses,
		"max_uses_per_user", r.MaxUsesPerUser,
		"used_count", r.UsedCount,
		"expires_at", nullableDateTime(r.ExpiresAt),
		"note", nullableStr(r.Note),
		"status", r.Status,
		"plan_id", nullableUint(r.PlanID),
		"plan_name", nullableStr(r.PlanName),
		"folder_id", nullableUint(r.FolderID),
		"created_at", nullableDateTime(r.CreatedAt),
	)
}

// queryRaw 取原始 query 值（用于 with_codes 这类布尔开关）。
func queryRaw(c *gin.Context, key string) any {
	raw, ok := c.GetQuery(key)
	if !ok {
		return nil
	}
	return raw
}

// loadFolder 解析 {folder} 并加载；不存在回「文件夹不存在」404（对齐 resolveFolder）。
func (h *Handler) loadFolder(c *gin.Context) (*Folder, bool) {
	id, ok := h.paramID(c, "folder")
	if ok {
		f, err := h.store.FolderByID(ctxOf(c), id)
		if err == nil {
			return f, true
		}
		if !NoRows(err) {
			h.failServer(c, "查询优惠码文件夹", err)
			return nil, false
		}
	}
	h.fail(c, "文件夹不存在", 404)
	return nil, false
}
