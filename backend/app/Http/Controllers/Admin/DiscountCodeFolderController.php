<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\DiscountCode;
use App\Models\DiscountCodeFolder;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 优惠码文件夹 CRUD（分组只影响后台展示，不参与码的校验/核销）。
 *
 * 删除语义：把文件夹里所有码的 folder_id 置 null（回到「未归档」），码本身一条都不删。
 */
class DiscountCodeFolderController extends Controller
{
    use ApiResponse;

    /** 文件夹名长度上限（与前端输入框 maxlength 保持一致）。 */
    private const NAME_MAX = 60;

    /**
     * 新建文件夹。
     */
    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'name' => ['required', 'string', 'max:' . self::NAME_MAX],
        ]);

        $folder = DiscountCodeFolder::create(['name' => trim($data['name'])]);

        return $this->success([
            'id' => $folder->id,
            'name' => $folder->name,
            'code_count' => 0,
        ], '已创建');
    }

    /**
     * 改名。
     */
    public function update(Request $request, string $folder): \Illuminate\Http\JsonResponse
    {
        $model = $this->resolveFolder($folder);
        if (! $model) {
            return $this->error('文件夹不存在', 404);
        }

        $data = $request->validate([
            'name' => ['required', 'string', 'max:' . self::NAME_MAX],
        ]);

        $model->fill(['name' => trim($data['name'])])->save();

        return $this->success(null, '保存成功');
    }

    /**
     * 删除文件夹：里面的码退回未归档，不删码。
     */
    public function destroy(string $folder): \Illuminate\Http\JsonResponse
    {
        $model = $this->resolveFolder($folder);
        if (! $model) {
            return $this->error('文件夹不存在', 404);
        }

        // 先摘码再删文件夹：删到一半失败时，码只会多出一批「未归档」，
        // 不会出现指向已删文件夹的孤儿 folder_id
        DiscountCode::where('folder_id', $model->id)->update(['folder_id' => null]);
        $model->delete();

        return $this->success(null, '已删除文件夹，其中优惠码已转为未归档');
    }

    private function resolveFolder(string $folder): ?DiscountCodeFolder
    {
        return DiscountCodeFolder::find($folder);
    }
}
