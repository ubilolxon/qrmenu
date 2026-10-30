import type { Text } from './types'

// Same list as internal/money: UZS is zero-decimal here even though ISO 4217
// says two, because tiyin are not used. price_minor is "the smallest unit
// actually used", so for UZS it is whole som.
const ZERO_DECIMAL = new Set(['UZS', 'JPY', 'KRW', 'VND', 'CLP', 'ISK', 'KZT'])

export function exponent(currency: string) {
  return ZERO_DECIMAL.has(currency.toUpperCase()) ? 0 : 2
}

export function formatMoney(minor: number, currency: string) {
  const exp = exponent(currency)
  const amount = minor / 10 ** exp
  return `${amount.toLocaleString('en-US', { minimumFractionDigits: exp, maximumFractionDigits: exp }).replace(/,/g, ' ')} ${currency}`
}

// "12.50" → 1250 for USD, "45000" → 45000 for UZS. Returns null for junk.
export function parseMoney(input: string, currency: string): number | null {
  const cleaned = input.replace(/\s/g, '').replace(',', '.')
  if (!/^\d+(\.\d+)?$/.test(cleaned)) return null
  return Math.round(Number(cleaned) * 10 ** exponent(currency))
}

export function toMajor(minor: number, currency: string) {
  return String(minor / 10 ** exponent(currency))
}

// Requested locale → default locale → first non-empty: the same fallback as i18n.Text.Get.
export function t(text: Text | null | undefined, locale: string, fallback?: string): string {
  if (!text) return ''
  return text[locale] || (fallback && text[fallback]) || Object.values(text).find(Boolean) || ''
}

export function timeAgo(iso: string) {
  const minutes = Math.floor((Date.now() - new Date(iso).getTime()) / 60000)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes} min ago`
  return `${Math.floor(minutes / 60)} h ${minutes % 60} min ago`
}

export const LOCALE_NAMES: Record<string, string> = { uz: "O'zbekcha", ru: 'Русский', en: 'English' }
