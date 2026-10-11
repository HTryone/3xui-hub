<?php

namespace App\Support;

/**
 * 节点地区识别：从节点名称里的关键词判断地区（两位代码）。
 *
 * 这张映射表原先内嵌在 SubscriptionService::addFlagToName() 里（订阅节点改名用）。
 * 仪表盘的地球也要用同一套判定，抽出来共用 —— 避免维护两份，也避免两边判定结果不一致
 * （订阅显示 HK、地球上却点不出来）。
 *
 * 判定规则与抽出来之前逐字一致：
 *   - 按声明顺序取**第一个**命中的关键词；
 *   - 一个都没命中返回 null（不猜）。
 *
 * 顺序有意义，不要重排：「中国香港」必须命中 HK 而不是 CN；「Singapore」必须命中
 * 新加坡而不是印度的 IN。
 */
class NodeRegion
{
    /** 关键词 => 两位地区代码。数组顺序即优先级。 */
    private const KEYWORDS = [
        '香港' => 'HK', 'HK' => 'HK', 'Hong Kong' => 'HK',
        '台湾' => 'TW', 'TW' => 'TW', 'Taiwan' => 'TW',
        '日本' => 'JP', 'JP' => 'JP', 'Japan' => 'JP',
        '韩国' => 'KR', 'KR' => 'KR', 'Korea' => 'KR',
        '新加坡' => 'SG', 'SG' => 'SG', 'Singapore' => 'SG',
        '美国' => 'US', 'US' => 'US', 'USA' => 'US', 'America' => 'US',
        '英国' => 'UK', 'UK' => 'UK', 'Britain' => 'UK',
        '德国' => 'DE', 'DE' => 'DE', 'Germany' => 'DE',
        '法国' => 'FR', 'FR' => 'FR', 'France' => 'FR',
        '加拿大' => 'CA', 'CA' => 'CA', 'Canada' => 'CA',
        '澳大利亚' => 'AU', 'AU' => 'AU', 'Australia' => 'AU',
        '印度' => 'IN', 'IN' => 'IN', 'India' => 'IN',
        '泰国' => 'TH', 'TH' => 'TH', 'Thailand' => 'TH',
        '马来西亚' => 'MY', 'MY' => 'MY', 'Malaysia' => 'MY',
        '越南' => 'VN', 'VN' => 'VN', 'Vietnam' => 'VN',
        '俄罗斯' => 'RU', 'RU' => 'RU', 'Russia' => 'RU',
        '巴西' => 'BR', 'BR' => 'BR', 'Brazil' => 'BR',
        '土耳其' => 'TR', 'TR' => 'TR', 'Turkey' => 'TR',
        '荷兰' => 'NL', 'NL' => 'NL', 'Netherlands' => 'NL',
        '中国' => 'CN', 'CN' => 'CN', 'China' => 'CN',
    ];

    /** 地区代码 => 旗帜 emoji。 */
    private const FLAGS = [
        'HK' => '🇭🇰', 'TW' => '🇹🇼', 'JP' => '🇯🇵', 'KR' => '🇰🇷',
        'SG' => '🇸🇬', 'US' => '🇺🇸', 'UK' => '🇬🇧', 'DE' => '🇩🇪',
        'FR' => '🇫🇷', 'CA' => '🇨🇦', 'AU' => '🇦🇺', 'IN' => '🇮🇳',
        'TH' => '🇹🇭', 'MY' => '🇲🇾', 'VN' => '🇻🇳', 'RU' => '🇷🇺',
        'BR' => '🇧🇷', 'TR' => '🇹🇷', 'NL' => '🇳🇱', 'CN' => '🇨🇳',
    ];

    /** 从节点名称识别地区代码；识别不出返回 null。 */
    public static function code(?string $name): ?string
    {
        $name = (string) $name;
        if (trim($name) === '') {
            return null;
        }

        foreach (self::KEYWORDS as $keyword => $code) {
            if (self::matches($name, $keyword)) {
                return $code;
            }
        }

        return null;
    }

    /**
     * 关键词是否命中节点名。
     *
     * 中文与多字母关键词走子串匹配（「香港01」这种必须能命中）。
     *
     * **两位字母代码必须自成一段**（前后不紧挨字母），否则会满屏误判：
     * 「lo·CA·l-verify」命中 CA、「A·US·tralia」命中 US、「R·US·sia」命中 US、
     * 「Ch·IN·a」命中 IN、「Ne·TH·erlands」命中 TH。
     * 数字和分隔符不算字母，所以 HK01 / JP-02 / 🇭🇰 HK01 这些常见写法照旧命中。
     */
    private static function matches(string $name, string $keyword): bool
    {
        if (preg_match('/^[A-Za-z]{2}$/', $keyword) === 1) {
            return preg_match('/(?<![A-Za-z])' . preg_quote($keyword, '/') . '(?![A-Za-z])/i', $name) === 1;
        }

        // strtoupper 是字节级、不动多字节汉字，中文关键词因此按原样匹配。
        return stripos(strtoupper($name), strtoupper($keyword)) !== false;
    }

    /** 地区代码对应的旗帜 emoji；无对应返回空串。 */
    public static function flag(string $code): string
    {
        return self::FLAGS[$code] ?? '';
    }
}
