import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { del, get, inRestaurant, patch, post, put } from '../lib/api'
import { formatMoney, parseMoney, t, toMajor } from '../lib/format'
import type { Category, Image, Item, Menu, Restaurant, Text } from '../lib/types'
import ImageUpload from '../components/ImageUpload'
import { ErrorBox, Field, TextField, Toggle, cleanText } from '../components/ui'
import { useRestaurant } from './RestaurantLayout'

// Query keys are scoped per restaurant so switching restaurants never shows
// the previous one's menu while the next loads.
const keys = {
  menus: (r: string) => ['menus', r],
  categories: (r: string, m: string) => ['categories', r, m],
  items: (r: string, c: string) => ['items', r, c],
  images: (r: string) => ['images', r],
}

export default function MenuEditor() {
  const restaurant = useRestaurant()
  const rid = restaurant.id
  const menus = useQuery({
    queryKey: keys.menus(rid),
    queryFn: () => get<{ menus: Menu[] }>(inRestaurant(rid, '/menus')).then((r) => r.menus),
  })
  const [selected, setSelected] = useState<string | null>(null)
  const current = menus.data?.find((m) => m.id === selected) ?? menus.data?.[0]

  return (
    <main className="page split">
      <aside className="sidebar">
        <h2>Menus</h2>
        <ErrorBox error={menus.error} />
        <ul className="nav-list">
          {menus.data?.map((m) => (
            <li key={m.id}>
              <button className={m.id === current?.id ? 'active' : ''} onClick={() => setSelected(m.id)}>
                {t(m.name, restaurant.default_locale) || 'Untitled'}
                {!m.is_active && <span className="tag warn">off</span>}
              </button>
            </li>
          ))}
        </ul>
        <QuickCreate
          placeholder="New menu, e.g. Lunch"
          path={inRestaurant(rid, '/menus')}
          locale={restaurant.default_locale}
          invalidate={keys.menus(rid)}
          onCreated={(m: Menu) => setSelected(m.id)}
        />
      </aside>
      <section className="content">
        {menus.data?.length === 0 && <div className="empty">Create your first menu on the left.</div>}
        {current && <MenuPanel key={current.id} restaurant={restaurant} menu={current} />}
      </section>
    </main>
  )
}

function MenuPanel({ restaurant, menu }: { restaurant: Restaurant; menu: Menu }) {
  const rid = restaurant.id
  const qc = useQueryClient()
  const categories = useQuery({
    queryKey: keys.categories(rid, menu.id),
    queryFn: () =>
      get<{ categories: Category[] }>(inRestaurant(rid, `/menus/${menu.id}/categories`)).then((r) => r.categories),
  })
  // One request for every image in the restaurant, so item rows can show
  // thumbnails without a request each.
  const images = useQuery({
    queryKey: keys.images(rid),
    queryFn: () => get<{ images: Image[] }>(inRestaurant(rid, '/images')).then((r) => r.images),
  })
  const imageByID = new Map(images.data?.map((i) => [i.id, i]))

  const update = useMutation({
    mutationFn: (body: object) => patch(inRestaurant(rid, `/menus/${menu.id}`), body),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.menus(rid) }),
  })
  const remove = useMutation({
    mutationFn: () => del(inRestaurant(rid, `/menus/${menu.id}`)),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.menus(rid) }),
  })

  return (
    <>
      <div className="row spread">
        <EditableName
          value={menu.name}
          locales={restaurant.locales}
          locale={restaurant.default_locale}
          onSave={(name) => update.mutate({ name })}
          heading
        />
        <div className="row">
          <Toggle checked={menu.is_active} label="Active" onChange={(is_active) => update.mutate({ is_active })} />
          <button
            className="danger ghost"
            onClick={() => confirm('Delete this menu and everything in it?') && remove.mutate()}
          >
            Delete menu
          </button>
        </div>
      </div>
      <ErrorBox error={update.error ?? remove.error ?? categories.error} />

      {categories.data?.map((c) => (
        <CategoryCard key={c.id} restaurant={restaurant} menuID={menu.id} category={c} imageByID={imageByID} />
      ))}
      {categories.data?.length === 0 && <div className="empty">No categories yet — add one below, e.g. "Salads".</div>}

      <div className="card subtle">
        <QuickCreate
          placeholder="New category"
          path={inRestaurant(rid, `/menus/${menu.id}/categories`)}
          locale={restaurant.default_locale}
          invalidate={keys.categories(rid, menu.id)}
        />
      </div>
    </>
  )
}

function CategoryCard({
  restaurant,
  menuID,
  category,
  imageByID,
}: {
  restaurant: Restaurant
  menuID: string
  category: Category
  imageByID: Map<string, Image>
}) {
  const rid = restaurant.id
  const qc = useQueryClient()
  const [editing, setEditing] = useState<Item | null>(null)
  const items = useQuery({
    queryKey: keys.items(rid, category.id),
    queryFn: () => get<{ items: Item[] }>(inRestaurant(rid, `/categories/${category.id}/items`)).then((r) => r.items),
  })
  const refreshCategories = () => qc.invalidateQueries({ queryKey: keys.categories(rid, menuID) })

  const update = useMutation({
    mutationFn: (body: object) => patch(inRestaurant(rid, `/categories/${category.id}`), body),
    onSuccess: refreshCategories,
  })
  const remove = useMutation({
    mutationFn: () => del(inRestaurant(rid, `/categories/${category.id}`)),
    onSuccess: refreshCategories,
  })

  return (
    <div className="card">
      <div className="row spread">
        <EditableName
          value={category.name}
          locales={restaurant.locales}
          locale={restaurant.default_locale}
          onSave={(name) => update.mutate({ name })}
        />
        <div className="row">
          <Toggle checked={category.is_active} label="Visible" onChange={(is_active) => update.mutate({ is_active })} />
          <button
            className="danger ghost small"
            onClick={() => confirm('Delete this category and its dishes?') && remove.mutate()}
          >
            Delete
          </button>
        </div>
      </div>
      <ErrorBox error={update.error ?? remove.error ?? items.error} />

      <ul className="items">
        {items.data?.map((item) => (
          <ItemRow
            key={item.id}
            restaurant={restaurant}
            item={item}
            image={item.image_id ? imageByID.get(item.image_id) : undefined}
            onEdit={() => setEditing(item)}
          />
        ))}
      </ul>

      <NewItem restaurant={restaurant} categoryID={category.id} />

      {editing && (
        <ItemEditor
          restaurant={restaurant}
          item={editing}
          image={editing.image_id ? imageByID.get(editing.image_id) : undefined}
          onClose={() => setEditing(null)}
        />
      )}
    </div>
  )
}

function ItemRow({
  restaurant,
  item,
  image,
  onEdit,
}: {
  restaurant: Restaurant
  item: Item
  image?: Image
  onEdit: () => void
}) {
  const qc = useQueryClient()
  // Sold out is the one menu write that happens mid-service, so it is a
  // one-tap toggle rather than buried in the edit form.
  const availability = useMutation({
    mutationFn: (is_available: boolean) =>
      put(inRestaurant(restaurant.id, `/items/${item.id}/availability`), { is_available }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.items(restaurant.id, item.category_id) }),
  })

  return (
    <li className={item.is_available ? '' : 'sold-out'}>
      {image?.urls.thumb ? <img src={image.urls.thumb} alt="" className="thumb" /> : <div className="thumb empty-thumb" />}
      <div className="grow">
        <strong>{t(item.name, restaurant.default_locale)}</strong>
        <div className="muted small">{t(item.description, restaurant.default_locale)}</div>
      </div>
      <span className="price">{formatMoney(item.price_minor, restaurant.currency)}</span>
      <Toggle
        checked={item.is_available}
        label={item.is_available ? 'In stock' : 'Sold out'}
        onChange={(v) => availability.mutate(v)}
      />
      <button className="ghost small" onClick={onEdit}>Edit</button>
    </li>
  )
}

function NewItem({ restaurant, categoryID }: { restaurant: Restaurant; categoryID: string }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [price, setPrice] = useState('')

  const create = useMutation({
    mutationFn: (price_minor: number) =>
      post(inRestaurant(restaurant.id, `/categories/${categoryID}/items`), {
        name: { [restaurant.default_locale]: name.trim() },
        price_minor,
      }),
    onSuccess: () => {
      setName('')
      setPrice('')
      qc.invalidateQueries({ queryKey: keys.items(restaurant.id, categoryID) })
    },
  })

  const parsed = parseMoney(price, restaurant.currency)

  function submit(e: FormEvent) {
    e.preventDefault()
    if (parsed !== null) create.mutate(parsed)
  }

  return (
    <form className="row inline-form" onSubmit={submit}>
      <input placeholder="New dish" value={name} onChange={(e) => setName(e.target.value)} required />
      <input
        placeholder={`Price, ${restaurant.currency}`}
        inputMode="decimal"
        value={price}
        onChange={(e) => setPrice(e.target.value)}
        className="narrow-input"
        required
      />
      <button disabled={create.isPending || parsed === null}>Add dish</button>
      <ErrorBox error={create.error} />
    </form>
  )
}

function ItemEditor({
  restaurant,
  item,
  image,
  onClose,
}: {
  restaurant: Restaurant
  item: Item
  image?: Image
  onClose: () => void
}) {
  const rid = restaurant.id
  const qc = useQueryClient()
  const [name, setName] = useState<Text>(item.name)
  const [description, setDescription] = useState<Text>(item.description ?? {})
  const [price, setPrice] = useState(toMajor(item.price_minor, restaurant.currency))
  const [prep, setPrep] = useState(item.prep_minutes?.toString() ?? '')
  const [tags, setTags] = useState(item.tags.join(', '))
  const [uploaded, setUploaded] = useState<Image | null>(null)
  const [clearImage, setClearImage] = useState(false)

  const shownImage = clearImage ? undefined : (uploaded ?? image)
  const priceMinor = parseMoney(price, restaurant.currency)

  const done = () => {
    qc.invalidateQueries({ queryKey: keys.items(rid, item.category_id) })
    qc.invalidateQueries({ queryKey: keys.images(rid) })
    onClose()
  }

  const save = useMutation({
    mutationFn: () =>
      patch(inRestaurant(rid, `/items/${item.id}`), {
        name: cleanText(name),
        description: cleanText(description),
        price_minor: priceMinor,
        tags: tags.split(',').map((s) => s.trim()).filter(Boolean),
        ...(prep.trim() ? { prep_minutes: Number(prep) } : { clear_prep_minutes: true }),
        ...(uploaded && !clearImage ? { image_id: uploaded.id } : {}),
        ...(clearImage ? { clear_image: true } : {}),
      }),
    onSuccess: done,
  })
  const remove = useMutation({
    mutationFn: () => del(inRestaurant(rid, `/items/${item.id}`)),
    onSuccess: done,
  })

  function submit(e: FormEvent) {
    e.preventDefault()
    save.mutate()
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <form className="modal card" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        <div className="row spread">
          <h2>Edit dish</h2>
          <button type="button" className="ghost" onClick={onClose}>✕</button>
        </div>
        <TextField label="Name" value={name} locales={restaurant.locales} onChange={setName} required={restaurant.default_locale} />
        <TextField label="Description" value={description} locales={restaurant.locales} onChange={setDescription} multiline />
        <div className="row">
          <Field label={`Price, ${restaurant.currency}`}>
            <input inputMode="decimal" value={price} onChange={(e) => setPrice(e.target.value)} required />
          </Field>
          <Field label="Prep time, min">
            <input type="number" min={0} value={prep} onChange={(e) => setPrep(e.target.value)} />
          </Field>
        </div>
        <Field label="Tags" hint="Comma separated, e.g. spicy, vegetarian">
          <input value={tags} onChange={(e) => setTags(e.target.value)} />
        </Field>
        <div className="field">
          <span className="field-label">Photo</span>
          <div className="row">
            {shownImage?.urls.card && <img src={shownImage.urls.card} alt="" className="preview" />}
            {shownImage && (
              <button type="button" className="ghost small" onClick={() => setClearImage(true)}>Remove photo</button>
            )}
          </div>
          <ImageUpload
            restaurantID={rid}
            onUploaded={(img) => {
              setUploaded(img)
              setClearImage(false)
            }}
          />
        </div>
        <ErrorBox error={save.error ?? remove.error} />
        <div className="row spread">
          <button
            type="button"
            className="danger ghost"
            onClick={() => confirm('Delete this dish?') && remove.mutate()}
          >
            Delete dish
          </button>
          <button className="primary" disabled={save.isPending || priceMinor === null}>Save</button>
        </div>
      </form>
    </div>
  )
}

// A name shown as text that turns into per-locale inputs on click.
function EditableName({
  value,
  locales,
  locale,
  onSave,
  heading,
}: {
  value: Text
  locales: string[]
  locale: string
  onSave: (v: Text) => void
  heading?: boolean
}) {
  const [draft, setDraft] = useState<Text | null>(null)
  const label = t(value, locale) || 'Untitled'

  if (!draft) {
    return (
      <button className="editable" onClick={() => setDraft(value)} title="Rename">
        {heading ? <h1>{label}</h1> : <h3>{label}</h3>}
      </button>
    )
  }
  return (
    <form
      className="grow"
      onSubmit={(e) => {
        e.preventDefault()
        onSave(cleanText(draft))
        setDraft(null)
      }}
    >
      <TextField label="Name" value={draft} locales={locales} onChange={setDraft} required={locale} />
      <div className="row">
        <button className="primary small">Save</button>
        <button type="button" className="ghost small" onClick={() => setDraft(null)}>Cancel</button>
      </div>
    </form>
  )
}

// A single-input form that creates a named record in the default locale.
function QuickCreate<T>({
  placeholder,
  path,
  locale,
  invalidate,
  onCreated,
}: {
  placeholder: string
  path: string
  locale: string
  invalidate: unknown[]
  onCreated?: (created: T) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const create = useMutation({
    mutationFn: () => post<T>(path, { name: { [locale]: name.trim() } }),
    onSuccess: (created) => {
      setName('')
      qc.invalidateQueries({ queryKey: invalidate })
      onCreated?.(created)
    },
  })

  return (
    <form
      className="row inline-form"
      onSubmit={(e) => {
        e.preventDefault()
        create.mutate()
      }}
    >
      <input placeholder={placeholder} value={name} onChange={(e) => setName(e.target.value)} required />
      <button disabled={create.isPending}>Add</button>
      <ErrorBox error={create.error} />
    </form>
  )
}
