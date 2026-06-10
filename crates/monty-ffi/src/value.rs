//! Value handle construction and inspection entry points.

use monty::{DictPairs, JsonMontyObject, MontyObject};
use num_bigint::BigInt;

use crate::{
    KIND_INVALID, MgBytes, MgDataclassRawArgs, MgDate, MgDateTime, MgError, MgRawPair, MgRawValue,
    MgTimeDelta, MgTimeZone, MgValue, as_bytes, ffi_error, guard, monty_date_from_raw,
    monty_datetime_from_raw, monty_timedelta_from_raw, monty_timezone_from_raw, object_kind,
    read_raw_pairs, read_raw_values, read_string_list, str_arg, string_list_value, text_for_raw,
    write_bytes, write_dict_pairs_raw, write_owned_bytes, write_raw_values, write_string,
    write_value_handle,
};

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_free(value: *mut MgValue) {
    if !value.is_null() {
        // SAFETY: value handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(value)) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_json(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let bytes = serde_json::to_vec(&JsonMontyObject(unsafe { &(*value).object }))
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        write_owned_bytes(out, bytes)
    })
}

#[unsafe(no_mangle)]
pub extern "C" fn mg_value_ellipsis() -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Ellipsis,
    }))
}

#[unsafe(no_mangle)]
pub extern "C" fn mg_value_none() -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::None,
    }))
}

#[unsafe(no_mangle)]
pub extern "C" fn mg_value_bool(value: u8) -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Bool(value != 0),
    }))
}

#[unsafe(no_mangle)]
pub extern "C" fn mg_value_int(value: i64) -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Int(value),
    }))
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_big_int(
    value_ptr: *const u8,
    value_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let value = str_arg(value_ptr, value_len)?;
        let integer = value
            .parse::<BigInt>()
            .map_err(|err| ffi_error("ValueError", err.to_string()))?;
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::BigInt(integer),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub extern "C" fn mg_value_float(value: f64) -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Float(value),
    }))
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_string(
    value_ptr: *const u8,
    value_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let value = str_arg(value_ptr, value_len)?.to_owned();
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::String(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_path(
    value_ptr: *const u8,
    value_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let value = str_arg(value_ptr, value_len)?.to_owned();
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Path(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_bytes(
    ptr: *const u8,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let bytes = as_bytes(ptr, len)?.to_vec();
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Bytes(bytes),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_list_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::List(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_tuple_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::Tuple(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_named_tuple_raw(
    type_name_ptr: *const u8,
    type_name_len: usize,
    field_names: *const crate::MgStr,
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let type_name = str_arg(type_name_ptr, type_name_len)?.to_owned();
        let field_names = read_string_list(field_names, len)?;
        let values = read_raw_values(values, len)?;
        write_value_handle(
            out,
            MontyObject::NamedTuple {
                type_name,
                field_names,
                values,
            },
        )
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_set_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::Set(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_frozen_set_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::FrozenSet(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dict_raw(
    pairs: *mut MgRawPair,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let pairs = read_raw_pairs(pairs, len)?;
        write_value_handle(out, MontyObject::Dict(DictPairs::from(pairs)))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_date(
    value: *const MgDate,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "date pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = unsafe { *value };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Date(monty_date_from_raw(value)),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_datetime(
    value: *const MgDateTime,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "datetime pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = monty_datetime_from_raw(unsafe { &*value })?;
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::DateTime(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timedelta(
    value: *const MgTimeDelta,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "timedelta pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = unsafe { *value };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::TimeDelta(monty_timedelta_from_raw(value)),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timezone(
    value: *const MgTimeZone,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "timezone pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = monty_timezone_from_raw(unsafe { &*value })?;
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::TimeZone(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_raw(
    args: *mut MgDataclassRawArgs,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "dataclass args pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &mut *args };
        let name = crate::as_str(args.name)?.to_owned();
        let field_names = read_string_list(args.field_names, args.field_count)?;
        let attrs = DictPairs::from(read_raw_pairs(args.attrs, args.attr_count)?);
        write_value_handle(
            out,
            MontyObject::Dataclass {
                name,
                type_id: args.type_id,
                field_names,
                attrs,
                frozen: args.frozen != 0,
            },
        )
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_function(
    name_ptr: *const u8,
    name_len: usize,
    doc_ptr: *const u8,
    doc_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let name = str_arg(name_ptr, name_len)?.to_owned();
        let doc = str_arg(doc_ptr, doc_len)?;
        let docstring = (!doc.is_empty()).then(|| doc.to_owned());
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Function { name, docstring },
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_exception(
    exc_type_ptr: *const u8,
    exc_type_len: usize,
    message_ptr: *const u8,
    message_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let (exc_type, arg) = crate::parse_exc_type(
            str_arg(exc_type_ptr, exc_type_len)?,
            str_arg(message_ptr, message_len)?,
        );
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Exception { exc_type, arg },
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_exception_type(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Exception { exc_type, .. } => write_string(out, &exc_type.to_string()),
            _ => Err(ffi_error("TypeError", "value is not an exception")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_exception_message(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Exception { arg, .. } => write_string(out, arg.as_deref().unwrap_or("")),
            _ => Err(ffi_error("TypeError", "value is not an exception")),
        }
    })
}

#[unsafe(no_mangle)]
pub const unsafe extern "C" fn mg_value_kind(value: *const MgValue) -> u32 {
    if value.is_null() {
        return KIND_INVALID;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    object_kind(unsafe { &(*value).object })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_bool_get(value: *const MgValue) -> u8 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Bool(value) => u8::from(*value),
        _ => 0,
    }
}

#[unsafe(no_mangle)]
#[allow(clippy::missing_const_for_fn)]
pub unsafe extern "C" fn mg_value_int_get(value: *const MgValue) -> i64 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Int(value) => *value,
        _ => 0,
    }
}

#[unsafe(no_mangle)]
#[allow(clippy::missing_const_for_fn)]
pub unsafe extern "C" fn mg_value_float_get(value: *const MgValue) -> f64 {
    if value.is_null() {
        return 0.0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Float(value) => *value,
        _ => 0.0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_text(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let text = text_for_raw(object);
        write_string(out, &text)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_bytes_get(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Bytes(bytes) => write_bytes(out, bytes),
            _ => Err(ffi_error("TypeError", "value is not bytes")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_date_get(
    value: *const MgValue,
    out: *mut MgDate,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "date output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::Date(date) = object else {
            return Err(ffi_error("TypeError", "value is not date"));
        };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = MgDate {
                year: date.year,
                month: date.month,
                day: date.day,
                _pad: [0; 2],
            };
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_datetime_get(
    value: *const MgValue,
    out: *mut MgDateTime,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "datetime output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::DateTime(datetime) = object else {
            return Err(ffi_error("TypeError", "value is not datetime"));
        };
        let mut raw = MgDateTime {
            timezone_name: MgBytes::empty(),
            year: datetime.year,
            microsecond: datetime.microsecond,
            offset_seconds: datetime.offset_seconds.unwrap_or_default(),
            month: datetime.month,
            day: datetime.day,
            hour: datetime.hour,
            minute: datetime.minute,
            second: datetime.second,
            has_offset: u8::from(datetime.offset_seconds.is_some()),
            has_timezone_name: u8::from(datetime.timezone_name.is_some()),
            _pad: 0,
        };
        if let Some(name) = &datetime.timezone_name {
            write_string(std::ptr::addr_of_mut!(raw.timezone_name), name)?;
        }
        // SAFETY: out is checked for null above.
        unsafe { *out = raw };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timedelta_get(
    value: *const MgValue,
    out: *mut MgTimeDelta,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "timedelta output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::TimeDelta(delta) = object else {
            return Err(ffi_error("TypeError", "value is not timedelta"));
        };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = MgTimeDelta {
                days: delta.days,
                seconds: delta.seconds,
                microseconds: delta.microseconds,
            };
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timezone_get(
    value: *const MgValue,
    out: *mut MgTimeZone,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "timezone output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::TimeZone(timezone) = object else {
            return Err(ffi_error("TypeError", "value is not timezone"));
        };
        let mut raw = MgTimeZone {
            name: MgBytes::empty(),
            offset_seconds: timezone.offset_seconds,
            has_name: u8::from(timezone.name.is_some()),
            _pad: [0; 3],
        };
        if let Some(name) = &timezone.name {
            write_string(std::ptr::addr_of_mut!(raw.name), name)?;
        }
        // SAFETY: out is checked for null above.
        unsafe { *out = raw };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_named_tuple_type_name(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::NamedTuple { type_name, .. } => write_string(out, type_name),
            _ => Err(ffi_error("TypeError", "value is not named tuple")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_named_tuple_field_names(
    value: *const MgValue,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::NamedTuple {
            field_names: names, ..
        } = object
        else {
            return Err(ffi_error("TypeError", "value is not named tuple"));
        };
        // SAFETY: out is checked for null above.
        unsafe { *out = string_list_value(names) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_name(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Dataclass { name, .. } => write_string(out, name),
            _ => Err(ffi_error("TypeError", "value is not dataclass")),
        }
    })
}

#[unsafe(no_mangle)]
pub const unsafe extern "C" fn mg_value_dataclass_type_id(value: *const MgValue) -> u64 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Dataclass { type_id, .. } => *type_id,
        _ => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_field_names(
    value: *const MgValue,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::Dataclass {
            field_names: names, ..
        } = object
        else {
            return Err(ffi_error("TypeError", "value is not dataclass"));
        };
        // SAFETY: out is checked for null above.
        unsafe { *out = string_list_value(names) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_frozen(value: *const MgValue) -> u8 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Dataclass { frozen, .. } => u8::from(*frozen),
        _ => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_len(value: *const MgValue) -> usize {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::List(values)
        | MontyObject::Tuple(values)
        | MontyObject::Set(values)
        | MontyObject::FrozenSet(values)
        | MontyObject::NamedTuple { values, .. } => values.len(),
        MontyObject::Dict(values) => values.len(),
        MontyObject::Dataclass { attrs, .. } => attrs.len(),
        _ => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_items_raw(
    value: *const MgValue,
    out: *mut MgRawValue,
    len: usize,
) -> i32 {
    guard(std::ptr::null_mut(), || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::List(values)
            | MontyObject::Tuple(values)
            | MontyObject::Set(values)
            | MontyObject::FrozenSet(values)
            | MontyObject::NamedTuple { values, .. } => {
                write_raw_values(out, len, values, "sequence item")
            }
            _ => Err(ffi_error("TypeError", "value is not a sequence")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_pairs_raw(
    value: *const MgValue,
    out: *mut MgRawPair,
    len: usize,
) -> i32 {
    guard(std::ptr::null_mut(), || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Dict(values) => write_dict_pairs_raw(out, len, values, "dict pair"),
            MontyObject::Dataclass { attrs, .. } => {
                write_dict_pairs_raw(out, len, attrs, "dataclass pair")
            }
            _ => Err(ffi_error("TypeError", "value is not a dict")),
        }
    })
}
