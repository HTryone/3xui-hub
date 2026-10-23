<?php

use App\Http\Controllers\Admin\AuthController as AdminAuthController;
use App\Http\Controllers\Admin\AsyncTaskController as AdminAsyncTaskController;
use App\Http\Controllers\Admin\BackupController as AdminBackupController;
use App\Http\Controllers\Admin\DiscountCodeController as AdminDiscountCodeController;
use App\Http\Controllers\Admin\DiscountCodeFolderController as AdminDiscountCodeFolderController;
use App\Http\Controllers\Admin\DomainController as AdminDomainController;
use App\Http\Controllers\Admin\EmailController as AdminEmailController;
use App\Http\Controllers\Admin\GeodataController as AdminGeodataController;
use App\Http\Controllers\Admin\HomeController as AdminHomeController;
use App\Http\Controllers\Admin\NodeController as AdminNodeController;
use App\Http\Controllers\Admin\OrderController as AdminOrderController;
use App\Http\Controllers\Admin\PanelColorsController as AdminPanelColorsController;
use App\Http\Controllers\Admin\PaymentController as AdminPaymentController;
use App\Http\Controllers\Admin\PlanController as AdminPlanController;
use App\Http\Controllers\Admin\SyncTrafficController as AdminSyncTrafficController;
use App\Http\Controllers\Admin\ThirdPartyController as AdminThirdPartyController;
use App\Http\Controllers\Admin\UserController as AdminUserController;
use App\Http\Controllers\Admin\SiteSettingController as AdminSiteSettingController;
use App\Http\Controllers\Admin\StatsController as AdminStatsController;
use App\Http\Controllers\Admin\SubscriptionSettingController as AdminSubscriptionSettingController;
use App\Http\Controllers\Admin\SystemStatusController as AdminSystemStatusController;
use App\Http\Controllers\Admin\TutorialController as AdminTutorialController;
use App\Http\Controllers\Admin\XrayInboundController as AdminXrayInboundController;
use App\Http\Controllers\Admin\XrayOutboundController as AdminXrayOutboundController;
use App\Http\Controllers\Admin\XrayRoutingController as AdminXrayRoutingController;
use App\Http\Controllers\Admin\NodeWarpController as AdminNodeWarpController;
use App\Http\Controllers\Admin\OutboundSubscriptionController as AdminOutboundSubscriptionController;
use App\Http\Controllers\Admin\MonitorController as AdminMonitorController;
use App\Http\Controllers\Admin\UserTrafficController as AdminUserTrafficController;
use Illuminate\Support\Facades\Route;

/*
|--------------------------------------------------------------------------
| 节点资产分发（xray 双通道 M0：资产随面板入库，节点只连面板，GitHub 零依赖）
| 全部公开端点——节点安装/注册前无凭据可鉴权
|--------------------------------------------------------------------------
*/
Route::get('/node-install.sh', [\App\Http\Controllers\Api\NodeAssetController::class, 'nodeInstall']);
Route::get('/node-agent.sh', [\App\Http\Controllers\Api\NodeAssetController::class, 'nodeAgent']);
Route::get('/node-bin/manifest.json', [\App\Http\Controllers\Api\NodeAssetController::class, 'manifest']);
// agent 二进制路由必须排在 {version}/{arch}.zip 之前（虽然 {arch}.zip 后缀模式不会吞它，双保险）
Route::get('/node-bin/agent/{arch}', [\App\Http\Controllers\Api\NodeAssetController::class, 'agentBinary']);
Route::get('/node-bin/geo/{file}', [\App\Http\Controllers\Api\NodeAssetController::class, 'geo'])
    ->where('file', '[a-zA-Z0-9._-]+\.dat');
Route::get('/node-bin/{version}/{arch}.zip', [\App\Http\Controllers\Api\NodeAssetController::class, 'binary']);

/*
|--------------------------------------------------------------------------
| Admin API 路由（session 鉴权，JSON；前缀 /admin-api，与 SPA 页面 /admin 分离）
|--------------------------------------------------------------------------
*/

Route::post('/admin-api/login', [AdminAuthController::class, 'login']);

Route::middleware('admin.auth')->prefix('admin-api')->group(function () {
    Route::post('/logout', [AdminAuthController::class, 'logout']);
    Route::get('/settings', [AdminAuthController::class, 'settings']);
    Route::post('/change-password', [AdminAuthController::class, 'changePassword']);
    Route::post('/update-username', [AdminAuthController::class, 'updateUsername']);

    // Google 2FA
    Route::post('/google2fa/generate', [AdminAuthController::class, 'google2faGenerate']);
    Route::post('/google2fa/enable', [AdminAuthController::class, 'google2faEnable']);
    Route::post('/google2fa/disable', [AdminAuthController::class, 'google2faDisable']);

    // M3 用户管理
    Route::get('/users', [AdminUserController::class, 'index']);
    Route::post('/users', [AdminUserController::class, 'store']);
    Route::get('/users/{user}', [AdminUserController::class, 'show']);
    Route::put('/users/{user}', [AdminUserController::class, 'update']);
    Route::delete('/users/{user}', [AdminUserController::class, 'destroy']);
    Route::post('/users/{user}/reset-traffic', [AdminUserController::class, 'resetTraffic']);
    Route::post('/users/{user}/reset-password', [AdminUserController::class, 'resetPassword']);
    Route::post('/users/{user}/protocol', [AdminUserController::class, 'switchProtocol']);
    Route::post('/users/{user}/renew', [AdminUserController::class, 'renew']);

    // M4 节点管理
    Route::get('/nodes', [AdminNodeController::class, 'index']);
    Route::post('/nodes', [AdminNodeController::class, 'store']);
    // 流量倍率（原始倍率 / 用户端展示开关）—— 必须排在 /nodes/{node} 参数路由之前，
    // 否则 multiplier 会被当成 node id 吞掉（同 /payments/protocols 的先例）
    Route::put('/nodes/multiplier/original', [AdminNodeController::class, 'updateOriginalMultiplier']);
    Route::put('/nodes/multiplier/display', [AdminNodeController::class, 'updateMultiplierDisplay']);
    // 第三方订阅（固定路径）—— 必须排在 /nodes/{node} 参数路由之前，否则 third-party 会被当成 node id 吞掉
    Route::get('/nodes/third-party', [AdminThirdPartyController::class, 'index']);
    Route::post('/nodes/third-party', [AdminThirdPartyController::class, 'store']);
    Route::put('/nodes/third-party/switch', [AdminThirdPartyController::class, 'switch']);
    Route::post('/nodes/third-party/{sub}/refresh', [AdminThirdPartyController::class, 'refresh']);
    Route::put('/nodes/third-party/{sub}', [AdminThirdPartyController::class, 'update']);
    Route::delete('/nodes/third-party/{sub}', [AdminThirdPartyController::class, 'destroy']);
    Route::get('/nodes/{node}', [AdminNodeController::class, 'show']);
    Route::put('/nodes/{node}', [AdminNodeController::class, 'update']);
    Route::delete('/nodes/{node}', [AdminNodeController::class, 'destroy']);
    Route::post('/nodes/{node}/test', [AdminNodeController::class, 'test']);
    Route::post('/nodes/{node}/upgrade-agent', [AdminNodeController::class, 'upgradeAgent']);
    Route::post('/nodes/probe-inbounds', [AdminNodeController::class, 'probeInbounds']);

    // xray 节点配置能力（入站 / 出站 / 路由 / WARP；仅 driver_type=xray 的节点可用，控制器内校验）
    Route::get('/nodes/{node}/xray-inbounds', [AdminXrayInboundController::class, 'index']);
    Route::post('/nodes/{node}/xray-inbounds', [AdminXrayInboundController::class, 'store']);
    // 密钥生成必须排在 {inbound} 参数路由之前（同 third-party 的先例）
    Route::post('/nodes/{node}/xray-inbounds/reality-keypair', [AdminXrayInboundController::class, 'realityKeypair']);
    // 批量删除（入站列表多选删除；走 POST 因为 DELETE 带 body 在部分网关/代理下会被丢）
    Route::post('/nodes/{node}/xray-inbounds/batch-delete', [AdminXrayInboundController::class, 'batchDestroy']);
    // Reality 目标探测（节点侧真实 TLS 握手，结果 JSON 回执；留空用内置候选）
    Route::post('/nodes/{node}/xray-inbounds/reality-scan', [AdminXrayInboundController::class, 'realityScan']);
    Route::put('/nodes/{node}/xray-inbounds/{inbound}', [AdminXrayInboundController::class, 'update']);
    Route::delete('/nodes/{node}/xray-inbounds/{inbound}', [AdminXrayInboundController::class, 'destroy']);

    Route::get('/nodes/{node}/xray-outbounds', [AdminXrayOutboundController::class, 'index']);
    Route::post('/nodes/{node}/xray-outbounds', [AdminXrayOutboundController::class, 'store']);
    Route::post('/nodes/{node}/xray-outbounds/import', [AdminXrayOutboundController::class, 'import']);
    // 出站 → 客户端分享链接（只读，不写配置）；wireguard 等无标准格式由后端返回「不支持」
    Route::get('/nodes/{node}/xray-outbounds/{outbound}/link', [AdminXrayOutboundController::class, 'shareLink']);
    Route::put('/nodes/{node}/xray-outbounds/{outbound}', [AdminXrayOutboundController::class, 'update']);
    Route::delete('/nodes/{node}/xray-outbounds/{outbound}', [AdminXrayOutboundController::class, 'destroy']);
    Route::post('/nodes/{node}/xray-outbounds/{outbound}/test', [AdminXrayOutboundController::class, 'test']);

    // 出站订阅（服务器池）：静态段（refresh）必须排在 {subscription} 参数路由之前
    Route::get('/nodes/{node}/xray-outbound-subscriptions', [AdminOutboundSubscriptionController::class, 'index']);
    Route::post('/nodes/{node}/xray-outbound-subscriptions', [AdminOutboundSubscriptionController::class, 'store']);
    Route::put('/nodes/{node}/xray-outbound-subscriptions/{subscription}', [AdminOutboundSubscriptionController::class, 'update']);
    Route::delete('/nodes/{node}/xray-outbound-subscriptions/{subscription}', [AdminOutboundSubscriptionController::class, 'destroy']);
    Route::post('/nodes/{node}/xray-outbound-subscriptions/{subscription}/refresh', [AdminOutboundSubscriptionController::class, 'refresh']);

    Route::get('/nodes/{node}/xray-routing', [AdminXrayRoutingController::class, 'index']);
    Route::post('/nodes/{node}/xray-routing', [AdminXrayRoutingController::class, 'store']);
    Route::post('/nodes/{node}/xray-routing/test', [AdminXrayRoutingController::class, 'test']);
    // 负载均衡器 + 基础设置：字面量段必须排在 {rule} 参数路由之前（同 reality-keypair 的先例）
    Route::post('/nodes/{node}/xray-routing/balancers', [AdminXrayRoutingController::class, 'storeBalancer']);
    Route::put('/nodes/{node}/xray-routing/balancers/{balancer}', [AdminXrayRoutingController::class, 'updateBalancer']);
    Route::delete('/nodes/{node}/xray-routing/balancers/{balancer}', [AdminXrayRoutingController::class, 'destroyBalancer']);
    Route::put('/nodes/{node}/xray-routing/settings', [AdminXrayRoutingController::class, 'updateSettings']);
    Route::put('/nodes/{node}/xray-routing/{rule}', [AdminXrayRoutingController::class, 'update']);
    Route::delete('/nodes/{node}/xray-routing/{rule}', [AdminXrayRoutingController::class, 'destroy']);

    Route::get('/nodes/{node}/warp', [AdminNodeWarpController::class, 'show']);
    Route::post('/nodes/{node}/warp/register', [AdminNodeWarpController::class, 'register']);
    Route::post('/nodes/{node}/warp/apply', [AdminNodeWarpController::class, 'apply']);
    Route::post('/nodes/{node}/warp/rotate', [AdminNodeWarpController::class, 'rotate']);
    Route::put('/nodes/{node}/warp', [AdminNodeWarpController::class, 'update']);
    Route::delete('/nodes/{node}/warp', [AdminNodeWarpController::class, 'destroy']);

    // 套餐管理
    Route::get('/plans', [AdminPlanController::class, 'index']);
    Route::post('/plans', [AdminPlanController::class, 'store']);
    Route::get('/plans/{plan}', [AdminPlanController::class, 'show']);
    Route::put('/plans/{plan}', [AdminPlanController::class, 'update']);
    Route::post('/plans/{plan}/deactivate', [AdminPlanController::class, 'deactivate']);
    Route::post('/plans/{plan}/activate', [AdminPlanController::class, 'activate']);

    // 订单管理
    Route::get('/orders', [AdminOrderController::class, 'index']);

    // 仪表盘统计（成交额）
    Route::get('/stats/revenue', [AdminStatsController::class, 'revenue']);

    // 机器流量监控（只读）：窗口内合计 / TOP 排行 / 趋势曲线。
    // 数据源是健康检查顺手落下的 L0 采样（monitor_samples），本组不做任何写入。
    Route::get('/monitor/summary', [AdminMonitorController::class, 'summary']);
    Route::get('/monitor/series', [AdminMonitorController::class, 'series']);

    // 用户×机器 流量（只读）：按用户排行 / 按机器排行 / 单用户按机器明细。
    // 数据源 user_traffic_hourly / user_traffic_daily（同步事务内累加），本组不做任何写入。
    Route::get('/traffic/user-ranking', [AdminUserTrafficController::class, 'userRanking']);
    Route::get('/traffic/node-ranking', [AdminUserTrafficController::class, 'nodeRanking']);
    Route::get('/traffic/users/{user}/nodes', [AdminUserTrafficController::class, 'userNodes']);

    // 流量同步
    Route::post('/sync-traffic', [AdminSyncTrafficController::class, 'sync']);

    // 异步任务
    Route::get('/async-tasks', [AdminAsyncTaskController::class, 'index']);
    Route::post('/async-tasks/{asyncTask}/retry', [AdminAsyncTaskController::class, 'retry']);

    // 备份管理
    Route::get('/backup/export', [AdminBackupController::class, 'export']);
    Route::post('/backup/preview', [AdminBackupController::class, 'preview']);
    Route::post('/backup/import', [AdminBackupController::class, 'import']);

    // 邮箱配置
    Route::get('/email', [AdminEmailController::class, 'show']);
    Route::put('/email', [AdminEmailController::class, 'save']);
    Route::post('/email/test', [AdminEmailController::class, 'test']);
    // 自动通知配置（开关全部默认关闭，管理员自行开启）
    Route::get('/email/notify', [AdminEmailController::class, 'notifyConfig']);
    Route::put('/email/notify', [AdminEmailController::class, 'saveNotifyConfig']);
    // 批量发信（群发/单发，限速默认关闭由管理员填上限）
    Route::post('/email/batch-send', [AdminEmailController::class, 'batchSend']);
    // 按月定时发信（管理员选每月第 N 天 HH:MM，到点自动发；默认关闭）
    Route::get('/email/schedule-mail', [AdminEmailController::class, 'scheduleMailConfig']);
    Route::put('/email/schedule-mail', [AdminEmailController::class, 'saveScheduleMailConfig']);
    // 发信日志
    Route::get('/email/logs', [AdminEmailController::class, 'mailLogs']);

    // 多域名管理
    Route::get('/domains', [AdminDomainController::class, 'index']);
    Route::post('/domains', [AdminDomainController::class, 'store']);
    Route::post('/domains/{id}/primary', [AdminDomainController::class, 'setPrimary']);
    Route::post('/domains/{id}/apply', [AdminDomainController::class, 'apply']);
    Route::post('/domains/{id}/renew', [AdminDomainController::class, 'renew']);
    Route::delete('/domains/{id}', [AdminDomainController::class, 'destroy']);

    // 支付配置管理
    Route::get('/payments', [AdminPaymentController::class, 'index']);
    Route::post('/payments', [AdminPaymentController::class, 'store']);
    // 协议清单（动态表单元数据）—— 必须排在 /payments/{payment} 之前，否则被参数路由吞掉
    Route::get('/payments/protocols', [AdminPaymentController::class, 'protocols']);
    Route::get('/payments/{payment}', [AdminPaymentController::class, 'show']);
    Route::put('/payments/{payment}', [AdminPaymentController::class, 'update']);
    Route::delete('/payments/{payment}', [AdminPaymentController::class, 'destroy']);

    // 自定义公开首页（HTML + CSS）
    Route::get('/home-custom', [AdminHomeController::class, 'show']);
    Route::put('/home-custom', [AdminHomeController::class, 'save']);
    Route::post('/home-custom/reset', [AdminHomeController::class, 'reset']);

    // 管理后台配色（自定义管理端的核心颜色，亮/暗两套）
    Route::get('/panel-colors', [AdminPanelColorsController::class, 'show']);
    Route::put('/panel-colors', [AdminPanelColorsController::class, 'save']);
    Route::post('/panel-colors/reset', [AdminPanelColorsController::class, 'reset']);

    // 站点信息配置
    Route::get('/site-settings', [AdminSiteSettingController::class, 'index']);
    Route::put('/site-settings', [AdminSiteSettingController::class, 'update']);

    // 订阅格式配置
    Route::get('/subscription-settings', [AdminSubscriptionSettingController::class, 'index']);
    Route::put('/subscription-settings', [AdminSubscriptionSettingController::class, 'update']);

    // 系统状态（操作日志 + 定时任务 + worker）
    Route::get('/system/status', [AdminSystemStatusController::class, 'status']);

    // Geo 数据（geoip.dat / geosite.dat 更新与分发，issue #40）
    Route::get('/geodata', [AdminGeodataController::class, 'index']);
    Route::post('/geodata/update', [AdminGeodataController::class, 'update']);
    Route::post('/geodata/dispatch', [AdminGeodataController::class, 'dispatch']);

    // 教程管理
    Route::get('/tutorials', [AdminTutorialController::class, 'index']);
    Route::post('/tutorials', [AdminTutorialController::class, 'store']);
    Route::put('/tutorials/{tutorial}', [AdminTutorialController::class, 'update']);
    Route::delete('/tutorials/{tutorial}', [AdminTutorialController::class, 'destroy']);

    // 优惠码 / 邀请机制
    Route::get('/discount-codes', [AdminDiscountCodeController::class, 'index']);
    Route::post('/discount-codes', [AdminDiscountCodeController::class, 'store']);
    // 批量删除（优惠码页多选删除；走 POST 避免 DELETE 带 body 被网关丢）
    Route::post('/discount-codes/batch-delete', [AdminDiscountCodeController::class, 'batchDestroy']);
    Route::put('/discount-codes/{code}', [AdminDiscountCodeController::class, 'update']);
    Route::delete('/discount-codes/{code}', [AdminDiscountCodeController::class, 'destroy']);

    // 优惠码文件夹（分组展示用；删除只摘码不删码）
    Route::post('/discount-code-folders', [AdminDiscountCodeFolderController::class, 'store']);
    Route::put('/discount-code-folders/{folder}', [AdminDiscountCodeFolderController::class, 'update']);
    Route::delete('/discount-code-folders/{folder}', [AdminDiscountCodeFolderController::class, 'destroy']);
    Route::get('/invite-settings', [AdminDiscountCodeController::class, 'inviteSettings']);
    Route::put('/invite-settings', [AdminDiscountCodeController::class, 'updateInviteSettings']);
});
