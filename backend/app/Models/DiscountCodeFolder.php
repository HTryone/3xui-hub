<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;

/**
 * 优惠码文件夹：管理员把优惠码分组归档用，纯展示层组织，不参与码的校验/核销。
 * 删除文件夹只把里面码的 folder_id 置 null，码本身保留（见 DiscountCodeFolderController::destroy）。
 */
class DiscountCodeFolder extends Model
{
    protected $fillable = ['name'];

    public function codes(): HasMany
    {
        return $this->hasMany(DiscountCode::class, 'folder_id');
    }
}
