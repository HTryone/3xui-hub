<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\AsyncTask;
use App\Services\AsyncTaskService;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use InvalidArgumentException;

class AsyncTaskController extends Controller
{
    use ApiResponse;

    public function __construct(private AsyncTaskService $tasks) {}

    public function index(): JsonResponse
    {
        $tasks = AsyncTask::query()
            ->latest('id')
            ->limit(100)
            ->get();

        return $this->success($tasks);
    }

    public function retry(AsyncTask $asyncTask): JsonResponse
    {
        try {
            $task = $this->tasks->retry($asyncTask);
        } catch (InvalidArgumentException $exception) {
            return $this->error($exception->getMessage());
        }

        return $this->success($task, '任务已重新派发');
    }
}