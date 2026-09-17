<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;

#[Fillable([
    'type',
    'status',
    'subject_type',
    'subject_id',
    'total',
    'completed',
    'failed',
    'attempts',
    'max_attempts',
    'error',
    'meta',
    'started_at',
    'finished_at',
])]
class AsyncTask extends Model
{
    public const STATUS_PENDING = 'pending';

    public const STATUS_RUNNING = 'running';

    public const STATUS_SUCCEEDED = 'succeeded';

    public const STATUS_PARTIAL = 'partial';

    public const STATUS_FAILED = 'failed';

    protected function casts(): array
    {
        return [
            'total' => 'integer',
            'completed' => 'integer',
            'failed' => 'integer',
            'attempts' => 'integer',
            'max_attempts' => 'integer',
            'meta' => 'array',
            'started_at' => 'datetime',
            'finished_at' => 'datetime',
        ];
    }

    public function items(): HasMany
    {
        return $this->hasMany(AsyncTaskItem::class, 'task_id');
    }
}