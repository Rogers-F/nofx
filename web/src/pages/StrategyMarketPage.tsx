import { useLanguage } from '../contexts/LanguageContext'
import { t } from '../i18n/translations'

// Strategy Market — embedded vergex.trade/explore.
//
// vergex.trade now lists the NOFX origins in its enforced
// `Content-Security-Policy: frame-ancestors 'self' https://nofxos.ai
// https://www.nofxos.ai http://127.0.0.1:3000 http://localhost:3000` for the
// /explore path, so cross-origin embedding works. The X-Frame-Options header
// is still SAMEORIGIN, but modern browsers prioritize the CSP
// `frame-ancestors` directive when both are present (per CSP Level 2).
//
// Mirrors the DataPage.tsx pattern (vergex.trade/trending).
export function StrategyMarketPage() {
  const { language } = useLanguage()

  return (
    <div className="h-[calc(100vh-64px)] w-full">
      <iframe
        src="https://vergex.trade/explore"
        title={t('strategyMarket', language) || 'Strategy Market'}
        className="h-full w-full border-0"
        allow="fullscreen; clipboard-write"
        referrerPolicy="strict-origin-when-cross-origin"
      />
    </div>
  )
}
