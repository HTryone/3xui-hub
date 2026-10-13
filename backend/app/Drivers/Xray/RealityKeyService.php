<?php

namespace App\Drivers\Xray;

/**
 * Reality / X25519 密钥工具。
 *
 * 生成格式与 `xray x25519` 输出完全兼容（base64url 无填充）：
 * 用 sodium_compat（无 sodium 扩展时纯 PHP 兜底、有扩展自动加速）生成
 * X25519 keypair；私钥按 xray 惯例 clamp 后存储。
 *
 * 兼容性已与 xray v26.3.27 对拍验证：PHP 生成的私钥经 `xray x25519 -i`
 * 推出的公钥与 PHP 侧计算完全一致。
 */
class RealityKeyService
{
    /**
     * 生成 X25519 密钥对。
     *
     * @return array{0:string,1:string} [privateKey, publicKey]，均为 base64url 无填充
     */
    public static function generateKeypair(): array
    {
        $keypair = \ParagonIE_Sodium_Compat::crypto_box_keypair();
        $sk = \ParagonIE_Sodium_Compat::crypto_box_secretkey($keypair);
        $pk = \ParagonIE_Sodium_Compat::crypto_box_publickey($keypair);

        // 对齐 xray x25519 输出形态：私钥 clamp（X25519 标量钳位，幂等；公钥配对不变）
        $sk[0] = chr(ord($sk[0]) & 248);
        $sk[31] = chr((ord($sk[31]) & 127) | 64);

        return [self::base64url($sk), self::base64url($pk)];
    }

    /** 随机 shortId（hex 串，xray reality shortIds 格式）。 */
    public static function shortId(int $bytes = 4): string
    {
        return bin2hex(random_bytes($bytes));
    }

    /**
     * 从既有私钥推导公钥（base64url 无填充；解析失败返回空串）。
     *
     * 用于对接只存 privateKey、不存 publicKey 的面板数据
     * （如 3x-ui v3.10 的 reality 入站 realitySettings）。
     * 推导口径已与 `xray x25519 -i` 实测对拍一致。
     */
    public static function publicKeyFromPrivateKey(string $privateKey): string
    {
        $privateKey = trim($privateKey);
        if ($privateKey === '') {
            return '';
        }

        $raw = base64_decode(
            strtr($privateKey, '-_', '+/') . str_repeat('=', (4 - strlen($privateKey) % 4) % 4),
            true
        );
        if ($raw === false || strlen($raw) !== 32) {
            return '';
        }

        return self::base64url(\ParagonIE_Sodium_Compat::crypto_scalarmult_base($raw));
    }

    /** base64url 无填充（xray 全系字符串格式）。 */
    public static function base64url(string $bin): string
    {
        return rtrim(strtr(base64_encode($bin), '+/', '-_'), '=');
    }
}
